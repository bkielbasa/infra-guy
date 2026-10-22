package s3

import (
	"archive/zip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const emptyBodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	Prefix          string
	UsePathStyle    bool
	Client          *http.Client
	Now             func() time.Time
}

type Cache struct {
	endpoint        *url.URL
	region          string
	bucket          string
	accessKeyID     string
	secretAccessKey string
	prefix          string
	usePathStyle    bool
	client          *http.Client
	now             func() time.Time
}

func NewFromEnv(getenv func(string) string) (*Cache, error) {
	if getenv == nil {
		return nil, nil
	}
	bucket := strings.TrimSpace(getenv("S3_BUCKET"))
	if bucket == "" {
		return nil, nil
	}
	endpoint := strings.TrimSpace(getenv("S3_ENDPOINT"))
	usePathStyle := endpoint != ""
	if raw := strings.TrimSpace(getenv("S3_USE_PATH_STYLE")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("S3_USE_PATH_STYLE: %w", err)
		}
		usePathStyle = parsed
	}
	return New(Config{
		Endpoint:        endpoint,
		Region:          strings.TrimSpace(getenv("S3_REGION")),
		Bucket:          bucket,
		AccessKeyID:     strings.TrimSpace(getenv("S3_ACCESS_KEY_ID")),
		SecretAccessKey: strings.TrimSpace(getenv("S3_SECRET_ACCESS_KEY")),
		Prefix:          strings.TrimSpace(getenv("S3_PREFIX")),
		UsePathStyle:    usePathStyle,
	})
}

func New(cfg Config) (*Cache, error) {
	bucket := strings.TrimSpace(cfg.Bucket)
	if bucket == "" {
		return nil, errors.New("s3 cache requires a bucket name")
	}
	if strings.TrimSpace(cfg.AccessKeyID) == "" || strings.TrimSpace(cfg.SecretAccessKey) == "" {
		return nil, errors.New("s3 cache requires S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY")
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = "us-east-1"
	}
	rawEndpoint := strings.TrimSpace(cfg.Endpoint)
	if rawEndpoint == "" {
		rawEndpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", region)
	}
	u, err := url.Parse(rawEndpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint %q", rawEndpoint)
	}
	prefix := strings.Trim(strings.TrimSpace(cfg.Prefix), "/")
	if prefix == "" {
		prefix = "workspaces"
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Cache{
		endpoint:        u,
		region:          region,
		bucket:          bucket,
		accessKeyID:     strings.TrimSpace(cfg.AccessKeyID),
		secretAccessKey: strings.TrimSpace(cfg.SecretAccessKey),
		prefix:          prefix,
		usePathStyle:    cfg.UsePathStyle,
		client:          client,
		now:             now,
	}, nil
}

func (c *Cache) Save(ctx context.Context, key, srcDir string) error {
	tmpFile, err := os.CreateTemp("", "infraguy-ws-*.zip")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	hasher := sha256.New()
	if err := zipDirectory(srcDir, io.MultiWriter(tmpFile, hasher)); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("zip workspace: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	info, err := os.Stat(tmpPath)
	if err != nil {
		return err
	}
	payloadHash := hex.EncodeToString(hasher.Sum(nil))

	bodyFile, err := os.Open(tmpPath)
	if err != nil {
		return err
	}
	defer bodyFile.Close()

	objURL := c.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, objURL, bodyFile)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.GetBody = func() (io.ReadCloser, error) {
		return os.Open(tmpPath)
	}
	req.Header.Set("Content-Type", "application/zip")
	c.signRequest(req, payloadHash)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("s3 put %s failed (status %d): %s", key, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Cache) Restore(ctx context.Context, key, destDir string) (bool, error) {
	objURL := c.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, objURL, nil)
	if err != nil {
		return false, err
	}
	c.signRequest(req, emptyBodySHA256)

	resp, err := c.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("s3 get %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("s3 get %s failed (status %d): %s", key, resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	tmpFile, err := os.CreateTemp("", "infraguy-ws-restore-*.zip")
	if err != nil {
		return false, err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	size, err := io.Copy(tmpFile, resp.Body)
	if err != nil {
		_ = tmpFile.Close()
		return false, fmt.Errorf("download workspace zip: %w", err)
	}
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		_ = tmpFile.Close()
		return false, err
	}
	defer tmpFile.Close()

	if err := unzipToDir(tmpFile, size, destDir); err != nil {
		return false, fmt.Errorf("unzip workspace: %w", err)
	}
	return true, nil
}

func (c *Cache) Delete(ctx context.Context, key string) error {
	objURL := c.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, objURL, nil)
	if err != nil {
		return err
	}
	c.signRequest(req, emptyBodySHA256)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("s3 delete %s: %w", key, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("s3 delete %s failed (status %d)", key, resp.StatusCode)
	}
}

func (c *Cache) objectKey(key string) string {
	clean := strings.Trim(strings.TrimSpace(key), "/")
	if !strings.HasSuffix(clean, ".zip") {
		clean += ".zip"
	}
	if c.prefix != "" {
		return path.Join(c.prefix, clean)
	}
	return clean
}

func (c *Cache) objectURL(key string) string {
	u := *c.endpoint
	objKey := c.objectKey(key)
	basePath := strings.TrimRight(u.Path, "/")
	if c.usePathStyle {
		u.Path = basePath + "/" + c.bucket + "/" + objKey
	} else {
		u.Host = c.bucket + "." + u.Host
		u.Path = basePath + "/" + objKey
	}
	return u.String()
}

func (c *Cache) signRequest(req *http.Request, payloadHash string) {
	now := c.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	type headerEntry struct {
		key string
		val string
	}
	var headers []headerEntry
	for k, vals := range req.Header {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "host" || lk == "content-type" || strings.HasPrefix(lk, "x-amz-") {
			headers = append(headers, headerEntry{
				key: lk,
				val: strings.TrimSpace(strings.Join(vals, ",")),
			})
		}
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].key < headers[j].key })

	var canonicalHeaders strings.Builder
	var signedNames []string
	for _, h := range headers {
		canonicalHeaders.WriteString(h.key)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(h.val)
		canonicalHeaders.WriteByte('\n')
		signedNames = append(signedNames, h.key)
	}
	signedHeaders := strings.Join(signedNames, ";")

	canonicalURI := canonicalS3Path(req.URL.Path)
	canonicalQuery := req.URL.Query().Encode()

	canonicalReq := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	credScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, c.region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credScope,
		sha256Hex([]byte(canonicalReq)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+c.secretAccessKey), dateStamp)
	kRegion := hmacSHA256(kDate, c.region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	auth := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.accessKeyID, credScope, signedHeaders, signature,
	)
	req.Header.Set("Authorization", auth)
}

func canonicalS3Path(p string) string {
	if p == "" {
		return "/"
	}
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = strings.ReplaceAll(url.PathEscape(part), "+", "%20")
	}
	res := strings.Join(parts, "/")
	if !strings.HasPrefix(res, "/") {
		res = "/" + res
	}
	return res
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func shouldSkipWorkspaceEntry(relPath, baseName string) bool {
	if relPath == ".home" || strings.HasPrefix(relPath, ".home/") {
		return true
	}
	if baseName == "~infraguy_backend_override.tf.json" || baseName == "tfplan" {
		return true
	}
	return false
}

func zipDirectory(srcDir string, dst io.Writer) error {
	cleanRoot := filepath.Clean(srcDir)
	zw := zip.NewWriter(dst)

	err := filepath.WalkDir(cleanRoot, func(fullPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(cleanRoot, fullPath)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if shouldSkipWorkspaceEntry(relSlash, d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = relSlash
		if d.IsDir() {
			hdr.Name += "/"
			_, err := zw.CreateHeader(hdr)
			return err
		}

		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(fullPath)
			if err != nil {
				return err
			}
			w, err := zw.CreateHeader(hdr)
			if err != nil {
				return err
			}
			_, err = io.WriteString(w, linkTarget)
			return err
		}

		if !info.Mode().IsRegular() {
			return nil
		}
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(fullPath)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

func unzipToDir(r io.ReaderAt, size int64, destDir string) error {
	cleanDest := filepath.Clean(destDir)
	if err := os.MkdirAll(cleanDest, 0o755); err != nil {
		return err
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return err
	}

	for _, zf := range zr.File {
		target := filepath.Join(cleanDest, filepath.FromSlash(zf.Name))
		rel, err := filepath.Rel(cleanDest, target)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe zip entry path %q", zf.Name)
		}
		relSlash := filepath.ToSlash(rel)
		if shouldSkipWorkspaceEntry(relSlash, filepath.Base(target)) {
			continue
		}

		mode := zf.Mode()
		if zf.FileInfo().IsDir() {
			perm := mode.Perm()
			if perm == 0 {
				perm = 0o755
			}
			if err := os.MkdirAll(target, perm); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		if mode&os.ModeSymlink != 0 {
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			linkBytes, readErr := io.ReadAll(io.LimitReader(rc, 4096))
			_ = rc.Close()
			if readErr != nil {
				return readErr
			}
			linkTarget := string(linkBytes)
			resolvedTarget := linkTarget
			if !filepath.IsAbs(resolvedTarget) {
				resolvedTarget = filepath.Join(filepath.Dir(target), resolvedTarget)
			}
			linkRel, err := filepath.Rel(cleanDest, filepath.Clean(resolvedTarget))
			if err != nil || linkRel == ".." || strings.HasPrefix(linkRel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("unsafe symlink target %q in %q", linkTarget, zf.Name)
			}
			_ = os.Remove(target)
			if err := os.Symlink(linkTarget, target); err != nil {
				return err
			}
			continue
		}

		perm := mode.Perm()
		if perm == 0 {
			perm = 0o644
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		outFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
		if err != nil {
			_ = rc.Close()
			return err
		}
		_, copyErr := io.Copy(outFile, rc)
		closeErr := outFile.Close()
		_ = rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		// Explicitly chmod in case umask stripped executable permissions on provider binaries.
		if err := os.Chmod(target, perm); err != nil {
			return err
		}
	}
	return nil
}

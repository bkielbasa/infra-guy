package httpapi

import (
	"net/http"

	"infra-guy/internal/domain/project"
)

type engineFormView struct {
	Engine                                           string
	IsKubernetes, IsAnsible, IsHelm                  bool
	K8sNamespace, ManifestPath                       string
	Kustomize, K8sPrune                              bool
	Playbook, Inventory, ExtraVars, RequirementsFile string
	Become                                           bool
	ReleaseName, HelmNamespace                       string
	Chart, RepoURL, ChartVersion                     string
	ValuesFiles, SetValues                           string
	Atomic, Wait, CreateNamespace                    bool
	ToolVersion                                      string
}

func engineFormFrom(ps *project.Snapshot) engineFormView {
	eng := project.DefaultEngine
	var cfg project.EngineConfig
	if ps != nil {
		if ps.Engine != "" {
			eng = ps.Engine
		}
		cfg = ps.EngineConfig
	}
	return engineFormView{
		Engine:           eng,
		IsKubernetes:     eng == project.EngineKubernetes,
		IsAnsible:        eng == project.EngineAnsible,
		IsHelm:           eng == project.EngineHelm,
		K8sNamespace:     cfg.K8sNamespace,
		ManifestPath:     cfg.ManifestPath,
		Kustomize:        cfg.Kustomize,
		K8sPrune:         cfg.K8sPrune,
		Playbook:         cfg.Playbook,
		Inventory:        cfg.Inventory,
		ExtraVars:        cfg.ExtraVars,
		RequirementsFile: cfg.RequirementsFile,
		Become:           cfg.Become,
		ReleaseName:      cfg.ReleaseName,
		HelmNamespace:    cfg.HelmNamespace,
		Chart:            cfg.Chart,
		RepoURL:          cfg.RepoURL,
		ChartVersion:     cfg.ChartVersion,
		ValuesFiles:      cfg.ValuesFiles,
		SetValues:        cfg.SetValues,
		Atomic:           cfg.Atomic,
		Wait:             cfg.Wait,
		CreateNamespace:  cfg.CreateNamespace,
		ToolVersion:      cfg.ToolVersion,
	}
}

func engineConfigFromRequest(r *http.Request) project.EngineConfig {
	on := func(name string) bool {
		v := r.FormValue(name)
		return v == "on" || v == "true" || v == "1"
	}
	return project.EngineConfig{
		K8sNamespace:     r.FormValue("k8s_namespace"),
		Kustomize:        on("kustomize"),
		K8sPrune:         on("k8s_prune"),
		ManifestPath:     r.FormValue("manifest_path"),
		Playbook:         r.FormValue("playbook"),
		Inventory:        r.FormValue("inventory"),
		ExtraVars:        r.FormValue("extra_vars"),
		RequirementsFile: r.FormValue("requirements_file"),
		Become:           on("become"),
		ReleaseName:      r.FormValue("release_name"),
		HelmNamespace:    r.FormValue("helm_namespace"),
		Chart:            r.FormValue("chart"),
		RepoURL:          r.FormValue("repo_url"),
		ChartVersion:     r.FormValue("chart_version"),
		ValuesFiles:      r.FormValue("values_files"),
		SetValues:        r.FormValue("set_values"),
		Atomic:           on("atomic"),
		Wait:             on("wait"),
		CreateNamespace:  on("create_namespace"),
		ToolVersion:      r.FormValue("tool_version"),
	}
}

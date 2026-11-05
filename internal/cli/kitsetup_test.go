package cli

import (
	"testing"

	"github.com/dagsommer/boks/internal/kit"
	"github.com/dagsommer/boks/internal/sandbox"
	"github.com/dagsommer/boks/internal/workspace"
)

func TestApplyKitCarriesSetupAndMixinEnv(t *testing.T) {
	spec := &kit.Spec{
		Kind:        kit.KindMixin,
		Name:        "m",
		Environment: &kit.Environment{Variables: map[string]string{"A": "1"}},
		Setup: &kit.Setup{
			Install: []kit.InstallCommand{{Command: "true"}},
			Files:   []kit.SetupFile{{Path: "/etc/x", Content: "c"}},
		},
	}
	f := &policyFlags{kitSpec: spec}
	cfg := sandbox.Config{Workspaces: []workspace.Workspace{{HostPath: "/h", GuestPath: "/h"}}}
	if err := f.applyKit(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Setup == nil || len(cfg.Setup.Install) != 1 || len(cfg.Setup.Files) != 1 {
		t.Fatalf("setup = %+v", cfg.Setup)
	}
	if len(cfg.Env) != 1 || cfg.Env[0] != "A=1" {
		t.Fatalf("env = %v", cfg.Env)
	}
}

func TestApplyKitRefusesAFileInsideTheClone(t *testing.T) {
	spec := &kit.Spec{Name: "m", Setup: &kit.Setup{Files: []kit.SetupFile{{Path: "/h/.env", Content: "c"}}}}
	f := &policyFlags{kitSpec: spec}
	cfg := sandbox.Config{Clone: true, Workspaces: []workspace.Workspace{{HostPath: "/h", GuestPath: "/h"}}}
	if err := f.applyKit(&cfg); err == nil {
		t.Fatal("accepted")
	}
}

func TestCheckWorkspacesRefusesNewOnesOnly(t *testing.T) {
	inv := invocation{
		name:       "s",
		info:       sandbox.Info{Workspaces: []sandbox.WorkspaceRef{{HostPath: "/a"}, {HostPath: "/b"}}},
		workspaces: []workspace.Workspace{{HostPath: "/a"}},
	}
	var c fixedConflicts
	checkWorkspaces(inv, &c)
	if len(c.entries) != 0 {
		t.Fatalf("a subset was refused: %+v", c.entries)
	}
	inv.workspaces = append(inv.workspaces, workspace.Workspace{HostPath: "/c"})
	checkWorkspaces(inv, &c)
	if len(c.entries) != 1 {
		t.Fatalf("a new workspace was not refused: %+v", c.entries)
	}
}

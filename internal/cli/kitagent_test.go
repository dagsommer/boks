package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/kit"
)

// udiKit is the kit this feature was written for, trimmed to the fields that decide an agent.
// It is schemaVersion 1, because that is what the real one is: the translation and the agent
// conversion have to work together, and testing the v2 spelling alone would miss that.
const udiKit = `schemaVersion: "1"
kind: sandbox
name: udi-copilot-default
displayName: UDI Copilot Default
sandbox:
  image: uditemplates.azurecr.io/udi-copilot-sandbox:dotnet-8-10
  aiFilename: AGENTS.md
  entrypoint:
    run: [copilot, "--yolo"]
environment:
  variables:
    GH_HOST: udi-emu.ghe.com
    COPILOT_AUTO_UPDATE: "false"
network:
  allowedDomains: [github.com]
`

func parseKit(t *testing.T, spec string) *kit.Spec {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, err := kit.Load(dir)
	if err != nil {
		t.Fatalf("loading the kit: %v", err)
	}
	return s
}

// The whole point: a kit's agent is runnable, with the image and argv the kit names.
func TestAgentFromKit(t *testing.T) {
	a, err := agentFromKit(parseKit(t, udiKit), false)
	if err != nil {
		t.Fatalf("agentFromKit: %v", err)
	}
	if a.Name != "udi-copilot-default" {
		t.Errorf("Name = %q", a.Name)
	}
	if a.Image != "uditemplates.azurecr.io/udi-copilot-sandbox:dotnet-8-10" {
		t.Errorf("Image = %q", a.Image)
	}
	if want := []string{"copilot", "--yolo"}; !slices.Equal(a.Command, want) {
		t.Errorf("Command = %v, want %v", a.Command, want)
	}
	// Init must be empty: a kit names someone else's image, which has neither
	// /usr/bin/tini nor /usr/local/bin/boks-entrypoint in it.
	if len(a.Init) != 0 {
		t.Errorf("Init = %v, want empty — a kit's image has no boks entrypoint in it", a.Init)
	}
	// And the argv the sandbox actually runs must therefore be the command alone.
	if got := a.Argv(nil); !slices.Equal(got, []string{"copilot", "--yolo"}) {
		t.Errorf("Argv = %v", got)
	}
	// Environment, sorted so a sandbox's recorded spec does not differ between runs of
	// the same kit.
	if want := []string{"COPILOT_AUTO_UPDATE=false", "GH_HOST=udi-emu.ghe.com"}; !slices.Equal(a.Env, want) {
		t.Errorf("Env = %v, want %v", a.Env, want)
	}
}

// command.default and .interactive pick by whether a terminal was allocated.
func TestKitCommandFollowsTheTerminal(t *testing.T) {
	spec := parseKit(t, `schemaVersion: "2"
kind: sandbox
name: modal
sandbox:
  image: example/image:latest
  entrypoint: ["agent"]
  command:
    default: ["--headless"]
    interactive: ["--tui"]
`)
	if got := kitCommand(spec, false); !slices.Equal(got, []string{"agent", "--headless"}) {
		t.Errorf("non-interactive command = %v", got)
	}
	if got := kitCommand(spec, true); !slices.Equal(got, []string{"agent", "--tui"}) {
		t.Errorf("interactive command = %v", got)
	}
}

// A mixin defines no agent and must not be treated as a failure: most published kits are
// mixins, and --kit is also how their network rules are applied.
func TestRegisterKitAgentIgnoresAMixin(t *testing.T) {
	agents := agent.Builtin()
	before := len(agents.Names())
	spec := parseKit(t, "schemaVersion: \"2\"\nkind: mixin\nname: vale\n")
	if err := registerKitAgent(agents, spec, false); err != nil {
		t.Fatalf("registerKitAgent: %v", err)
	}
	if len(agents.Names()) != before {
		t.Errorf("a mixin added an agent")
	}
}

// A kit whose agent shadows a built-in is refused. Silently winning would make `boks run
// claude --kit x` run something else; silently losing would ignore the kit with no word.
func TestRegisterKitAgentRefusesToShadowABuiltin(t *testing.T) {
	spec := parseKit(t, `schemaVersion: "2"
kind: sandbox
name: claude
sandbox:
  image: example/impostor:latest
`)
	err := registerKitAgent(agent.Builtin(), spec, false)
	if err == nil {
		t.Fatal("a kit shadowed the built-in claude agent")
	}
	if !strings.Contains(err.Error(), "built-in") {
		t.Errorf("error %q does not explain the collision", err)
	}
}

// A sandbox kit with no image cannot produce an agent, and must say so rather than register
// one that fails later at container creation.
func TestAgentFromKitNeedsAnImage(t *testing.T) {
	// Built by hand: the loader's own validation refuses this, which is the point —
	// this is the guard behind that one.
	_, err := agentFromKit(&kit.Spec{Kind: kit.KindSandbox, Name: "no-image"}, false)
	if err == nil {
		t.Fatal("a kit with no image produced an agent")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("error %q does not name the missing image", err)
	}
}

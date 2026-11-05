package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/sandbox"
)

type resetRecord struct {
	removed []string
	stopped []string
	forgot  []string
	wiped   bool
}

func fakeResetOps(rec *resetRecord, terminal bool) resetOps {
	return resetOps{
		list: func(context.Context, string) ([]sandbox.Info, error) {
			return []sandbox.Info{{Name: "a"}, {Name: "b"}}, nil
		},
		remove: func(_ context.Context, _, name string, force bool) error {
			if !force {
				panic("reset must remove running sandboxes")
			}
			rec.removed = append(rec.removed, name)
			return nil
		},
		stopNetwork: func(_, name string) error { rec.stopped = append(rec.stopped, name); return nil },
		forget:      func(_, name string) error { rec.forgot = append(rec.forgot, name); return nil },
		networks:    func(string) []string { return []string{"a", "orphan"} },
		wipeSecrets: func(context.Context) ([]string, error) { rec.wiped = true; return []string{"x"}, nil },
		isTerminalIn: func(Env) bool {
			return terminal
		},
	}
}

func runReset(t *testing.T, ops resetOps, stdin string, args ...string) (string, string, error) {
	t.Helper()
	purgeState(t)
	var out, errOut bytes.Buffer
	env := Env{Args: args, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut}
	dev := &devFlags{address: "x.sock"}
	cmd := newResetCommandWith(env, dev, ops)
	cmd.SetArgs(args)
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestResetForceRemovesEverything(t *testing.T) {
	var rec resetRecord
	out, _, err := runReset(t, fakeResetOps(&rec, false), "", "--force")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rec.removed, ",") != "a,b" {
		t.Errorf("removed = %v", rec.removed)
	}
	if strings.Join(rec.stopped, ",") != "a,b,orphan" || strings.Join(rec.forgot, ",") != "a,b,orphan" {
		t.Errorf("networks: stopped %v forgot %v", rec.stopped, rec.forgot)
	}
	if !rec.wiped {
		t.Error("secrets were kept")
	}
	if !strings.Contains(out, "removed a") {
		t.Errorf("stdout = %q", out)
	}
}

func TestResetPreserveSecrets(t *testing.T) {
	var rec resetRecord
	if _, _, err := runReset(t, fakeResetOps(&rec, false), "", "--force", "--preserve-secrets"); err != nil {
		t.Fatal(err)
	}
	if rec.wiped {
		t.Error("--preserve-secrets still wiped the credentials")
	}
	if len(rec.removed) != 2 {
		t.Errorf("removed = %v", rec.removed)
	}
}

func TestResetRefusesWithoutTerminal(t *testing.T) {
	var rec resetRecord
	_, _, err := runReset(t, fakeResetOps(&rec, false), "y\n")
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v, want a refusal naming --force", err)
	}
	if len(rec.removed) != 0 || rec.wiped {
		t.Error("something was removed without confirmation")
	}
}

func TestResetConfirmationOnTerminal(t *testing.T) {
	var rec resetRecord
	if _, _, err := runReset(t, fakeResetOps(&rec, true), "n\n"); err == nil {
		t.Fatal("answering n went ahead")
	}
	if len(rec.removed) != 0 {
		t.Error("removed despite n")
	}
	if _, _, err := runReset(t, fakeResetOps(&rec, true), "y\n"); err != nil {
		t.Fatal(err)
	}
	if len(rec.removed) != 2 || !rec.wiped {
		t.Errorf("rec = %+v", rec)
	}
}

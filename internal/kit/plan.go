package kit

import (
	"fmt"
	"sort"
	"strings"
)

// Stage is one step of the order a sandbox's kit content is applied in.
type Stage struct {
	Name    string
	Entries []PlanEntry
}

// PlanEntry is one thing a stage does, and the kit it comes from.
type PlanEntry struct {
	Kit  string
	What string
}

// Plan lists what applying the members does, stage by stage, in the order of docs/kits-design.md
// §1.4 (the REF contract): network and environment, files/home, setup.install, setup.files,
// setup.startup, then files/workspace. Within a stage, entries follow the order of the members.
//
// An entry that a later kit overrides is still listed, marked, so the list shows what each kit
// asked for and what won rather than only the result.
func Plan(members []Member) []Stage {
	stages := []Stage{
		{Name: "network and environment"},
		{Name: "files/home"},
		{Name: "setup.install"},
		{Name: "setup.files"},
		{Name: "setup.startup"},
		{Name: "files/workspace"},
	}
	add := func(i int, kit, what string) {
		stages[i].Entries = append(stages[i].Entries, PlanEntry{kit, what})
	}

	// Which kit's value of each variable and file wins: the last one.
	winnerEnv, winnerHome, winnerWork := map[string]string{}, map[string]string{}, map[string]string{}
	for _, m := range members {
		s := m.Spec
		if s.Environment != nil {
			for k := range s.Environment.Variables {
				winnerEnv[k] = s.Name
			}
		}
		if s.Tree != nil {
			for _, f := range s.Tree.Home {
				winnerHome[f.Path] = s.Name
			}
			for _, f := range s.Tree.Workspace {
				winnerWork[f.Path] = s.Name
			}
		}
	}
	over := func(winner map[string]string, key, kit string) string {
		if w := winner[key]; w != kit {
			return " (overridden by " + w + ")"
		}
		return ""
	}

	for _, m := range members {
		s := m.Spec
		if s.Permissions != nil && s.Permissions.Network != nil {
			for _, a := range s.Permissions.Network.Allow {
				add(0, s.Name, "allow "+a)
			}
			for _, d := range s.Permissions.Network.Deny {
				add(0, s.Name, "deny "+d)
			}
		}
		if s.Environment != nil {
			keys := make([]string, 0, len(s.Environment.Variables))
			for k := range s.Environment.Variables {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				add(0, s.Name, "env "+k+"="+s.Environment.Variables[k]+over(winnerEnv, k, s.Name))
			}
		}
		if s.Tree != nil {
			for _, f := range s.Tree.Home {
				add(1, s.Name, fmt.Sprintf("~/%s (%d bytes)%s", f.Path, len(f.Data), over(winnerHome, f.Path, s.Name)))
			}
		}
		if s.Setup != nil {
			for _, c := range s.Setup.Install {
				user := c.User
				if user == "" {
					user = "root"
				}
				add(2, s.Name, fmt.Sprintf("run as %s: %s", user, c.Command))
			}
			for _, f := range s.Setup.Files {
				note := ""
				if f.OnlyIfMissing {
					note = " (only if missing)"
				}
				add(3, s.Name, "write "+f.Path+note)
			}
			for _, c := range s.Setup.Startup {
				note := ""
				if c.Background {
					note = " (background)"
				}
				add(4, s.Name, "start "+strings.Join(c.Command, " ")+note)
			}
		}
		if s.Tree != nil {
			for _, f := range s.Tree.Workspace {
				add(5, s.Name, fmt.Sprintf("<workspace>/%s (%d bytes, never overwrites)%s",
					f.Path, len(f.Data), over(winnerWork, f.Path, s.Name)))
			}
		}
	}
	return stages
}

// RenderPlan writes a plan as text, one stage per block, skipping stages with nothing in them.
func RenderPlan(stages []Stage) string {
	var b strings.Builder
	n := 0
	for _, st := range stages {
		if len(st.Entries) == 0 {
			continue
		}
		n++
		fmt.Fprintf(&b, "%d. %s\n", n, st.Name)
		for _, e := range st.Entries {
			fmt.Fprintf(&b, "   [%s] %s\n", e.Kit, e.What)
		}
	}
	if n == 0 {
		return "nothing to apply\n"
	}
	return b.String()
}

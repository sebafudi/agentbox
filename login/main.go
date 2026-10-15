package main

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
)

func main() {
	defer mouseZones.Close()
	if len(os.Args) > 1 && os.Args[1] == "--refresh" {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		s := collect(ctx)
		if s.Err != "" {
			fmt.Fprintln(os.Stderr, s.Err)
			os.Exit(1)
		}
		if err := saveCache(s); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	finder := len(os.Args) > 1 && os.Args[1] == "--find-agent"
	scheduled := len(os.Args) > 1 && os.Args[1] == "--scheduled-jobs"
	if len(os.Args) > 1 && !finder && !scheduled {
		fmt.Fprintln(os.Stderr, "usage: agentbox-login [--find-agent|--scheduled-jobs|--refresh]")
		os.Exit(1)
	}
	notice := ""
	for {
		initial := initialModel(finder, notice)
		if scheduled {
			initial.mode = "scheduled"
			initial.scheduled.returnMode = "home"
			initial.scheduled.queryFocus = true
			initial.scheduled.query.Focus()
			initial.scheduled.notice = notice
		}
		program := tea.NewProgram(initial)
		result, err := program.Run()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		m := result.(model)
		scheduled = m.mode == "scheduled"
		switch m.action.Verb {
		case "logout":
			if finder {
				return
			}
			os.Exit(2)
		case "plain-shell":
			return
		case "":
			if finder {
				return
			}
			os.Exit(2)
		default:
			err = runAction(m.action)
			if finder {
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				return
			}
			if err != nil {
				notice = "Could not open session: " + err.Error()
			} else {
				notice = ""
			}
		}
	}
}

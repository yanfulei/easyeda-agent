package app

// PCB length constraints — differential pairs + equal-length net groups (#176).
//
// `pcb report` could always MEASURE these (skew / spread), but nothing could
// CREATE one, so on any board driven purely by this CLI the report's
// differentialPairs / equalLengthNetGroups arrays stayed empty forever and the
// measurement was dead weight. These subcommands close that loop: declare the
// constraints before routing (P7), then read the measurement back afterwards.
//
// Every mutating action here pre-validates net names against the board and
// reads back after the write — see the handlers in extension/src/actions.ts.

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// splitNets parses a CSV net list, trimming blanks. Net names are taken
// verbatim otherwise: EasyEDA net names are case-sensitive and may contain
// characters we must not normalize away.
func splitNets(raw string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--nets is empty; pass a comma-separated net list, e.g. --nets A0,A1,A2 (see `easyeda pcb nets`)")
	}
	return out, nil
}

// addPcbConstraintCmds mounts `pcb diff-pair` and `pcb eq-group` onto the pcb
// command. Split into its own file to keep cmd_pcb.go navigable.
func addPcbConstraintCmds(pcb *cobra.Command, cfg *appConfig, window *string, stdout, stderr io.Writer) {
	// ── pcb diff-pair ─────────────────────────────────────────────────────
	dp := &cobra.Command{
		Use:   "diff-pair",
		Short: "Differential pair constraints (create/list/rename/delete)",
		Long: `Bind two nets as a differential pair so EasyEDA's DRC treats them as a pair
and ` + "`easyeda pcb report`" + ` can measure their skew (|lenP - lenN|).

Declare pairs BEFORE routing (P7): the constraint is what makes the router and
DRC aware of the pairing, and what makes the post-route skew number meaningful.

Net names are pre-validated against the board — a pair pointing at a net that
isn't on this PCB is refused before anything is written (` + "`easyeda pcb nets`" + `
lists the real ones; they are case-sensitive).`,
		Example: `  easyeda pcb diff-pair create --name USB0 --positive USB_DP --negative USB_DM
  easyeda pcb diff-pair list
  easyeda pcb diff-pair rename --name USB0 --to USB
  easyeda pcb diff-pair delete --name USB`,
	}

	dp.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List differential pairs on the active PCB",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return dispatch(cfg, "pcb.constraint.list", *window, nil, stdout, stderr)
		},
	})

	{
		var name, pos, neg string
		c := &cobra.Command{
			Use:   "create",
			Short: "Create a differential pair binding two nets",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				return dispatch(cfg, "pcb.differential_pair.create", *window, map[string]any{
					"name": name, "positiveNet": pos, "negativeNet": neg,
				}, stdout, stderr)
			},
		}
		c.Flags().StringVar(&name, "name", "", "constraint name, e.g. USB0 (required)")
		c.Flags().StringVar(&pos, "positive", "", "positive net, e.g. USB_DP (required)")
		c.Flags().StringVar(&neg, "negative", "", "negative net, e.g. USB_DM (required)")
		_ = c.MarkFlagRequired("name")
		_ = c.MarkFlagRequired("positive")
		_ = c.MarkFlagRequired("negative")
		dp.AddCommand(c)
	}

	{
		var name, to string
		c := &cobra.Command{
			Use:   "rename",
			Short: "Rename a differential pair",
			Long: `Rename a differential pair constraint.

Renaming is the ONLY modify operation the platform exposes on a pair — to change
which nets it binds, delete it and create it again.`,
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				return dispatch(cfg, "pcb.differential_pair.rename", *window, map[string]any{
					"name": name, "newName": to,
				}, stdout, stderr)
			},
		}
		c.Flags().StringVar(&name, "name", "", "current name (required)")
		c.Flags().StringVar(&to, "to", "", "new name (required)")
		_ = c.MarkFlagRequired("name")
		_ = c.MarkFlagRequired("to")
		dp.AddCommand(c)
	}

	{
		var name string
		c := &cobra.Command{
			Use:   "delete",
			Short: "Delete a differential pair by name",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				return dispatch(cfg, "pcb.differential_pair.delete", *window, map[string]any{"name": name}, stdout, stderr)
			},
		}
		c.Flags().StringVar(&name, "name", "", "constraint name (required)")
		_ = c.MarkFlagRequired("name")
		dp.AddCommand(c)
	}

	pcb.AddCommand(dp)

	// ── pcb eq-group ──────────────────────────────────────────────────────
	eq := &cobra.Command{
		Use:   "eq-group",
		Short: "Equal-length net group constraints (create/list/add/delete)",
		Long: `Group nets that must match in routed length (a parallel bus, a memory
address group), so ` + "`easyeda pcb report`" + ` can measure the group's spread
(max - min across members).

Needs at least 2 nets — a one-net group constrains nothing. Net names are
pre-validated against the board (` + "`easyeda pcb nets`" + `).`,
		Example: `  easyeda pcb eq-group create --name DDR_ADDR --nets A0,A1,A2
  easyeda pcb eq-group add --name DDR_ADDR --nets A3,A4
  easyeda pcb eq-group list
  easyeda pcb eq-group delete --name DDR_ADDR`,
	}

	eq.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List equal-length groups on the active PCB",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return dispatch(cfg, "pcb.constraint.list", *window, nil, stdout, stderr)
		},
	})

	{
		var name, netsRaw string
		c := &cobra.Command{
			Use:   "create",
			Short: "Create an equal-length group from 2+ nets",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				nets, err := splitNets(netsRaw)
				if err != nil {
					return err
				}
				return dispatch(cfg, "pcb.equal_length_group.create", *window, map[string]any{
					"name": name, "nets": nets,
				}, stdout, stderr)
			},
		}
		c.Flags().StringVar(&name, "name", "", "group name, e.g. DDR_ADDR (required)")
		c.Flags().StringVar(&netsRaw, "nets", "", "member nets — CSV: A0,A1,A2 (required, 2+)")
		_ = c.MarkFlagRequired("name")
		_ = c.MarkFlagRequired("nets")
		eq.AddCommand(c)
	}

	{
		var name, netsRaw string
		c := &cobra.Command{
			Use:   "add",
			Short: "Add nets to an existing equal-length group",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				nets, err := splitNets(netsRaw)
				if err != nil {
					return err
				}
				return dispatch(cfg, "pcb.equal_length_group.add_nets", *window, map[string]any{
					"name": name, "nets": nets,
				}, stdout, stderr)
			},
		}
		c.Flags().StringVar(&name, "name", "", "existing group name (required)")
		c.Flags().StringVar(&netsRaw, "nets", "", "nets to add — CSV (required); already-member nets are skipped")
		_ = c.MarkFlagRequired("name")
		_ = c.MarkFlagRequired("nets")
		eq.AddCommand(c)
	}

	{
		var name string
		c := &cobra.Command{
			Use:   "delete",
			Short: "Delete an equal-length group by name",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				return dispatch(cfg, "pcb.equal_length_group.delete", *window, map[string]any{"name": name}, stdout, stderr)
			},
		}
		c.Flags().StringVar(&name, "name", "", "group name (required)")
		_ = c.MarkFlagRequired("name")
		eq.AddCommand(c)
	}

	pcb.AddCommand(eq)
}

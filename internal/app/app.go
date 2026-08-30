package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
	"github.com/zhoushoujianwork/easyeda-agent/internal/version"
)

// Run is the main entry point called by main.go.
// It returns 0 on success, 1 on any error.
func Run(args []string, stdout, stderr io.Writer) int {
	root := newRootCmd(stdout, stderr)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(context.Background()); err != nil {
		// A command that wants a specific exit code (e.g. `update --check
		// --exit-code` signalling "updates available") has already printed its
		// report — surface the code only.
		var ec exitCodeError
		if errors.As(err, &ec) {
			return ec.code
		}
		// errActionFailed / errQuiet mean the response was already printed to
		// stdout; no further message needed. All other errors get printed here.
		if !errors.Is(err, errActionFailed) && !errors.Is(err, errQuiet) {
			fmt.Fprintln(stderr, err)
		}
		return 1
	}
	return 0
}

// errQuiet fails the command without printing anything extra — for commands
// that already emitted a machine-readable report on stdout.
var errQuiet = errors.New("command failed (details already reported)")

// exitCodeError carries a specific process exit code out of a RunE, for
// gate-able commands whose "non-zero" is a verdict rather than a failure.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	cfg := &appConfig{
		host:  defaultHost,
		ports: fmt.Sprintf("%d-%d", defaultPortStart, defaultPortEnd),
	}

	root := &cobra.Command{
		Use:   "easyeda",
		Short: version.Name + " — AI-native EasyEDA Pro automation layer",
		// SilenceUsage: don't dump usage on every error.
		// SilenceErrors: we handle printing ourselves so we can suppress
		// errActionFailed without also suppressing "unknown command" etc.
		SilenceUsage:  true,
		SilenceErrors: true,
		// Setting Version enables `--version`; pre-registering the flag below
		// adds the `-v` shorthand. Same output as the `version` subcommand.
		Version: version.Version,
	}
	root.SetVersionTemplate(version.Name + " {{.Version}}\n")
	root.Flags().BoolP("version", "v", false, "print version and exit")

	root.PersistentFlags().StringVar(&cfg.host, "host", defaultHost,
		"daemon host")
	root.PersistentFlags().StringVar(&cfg.ports, "ports",
		fmt.Sprintf("%d-%d", defaultPortStart, defaultPortEnd),
		"daemon port range (start-end)")
	root.PersistentFlags().StringVar(&cfg.project, "project", "",
		"route by project name/uuid instead of --window (survives windowId churn)")
	root.PersistentFlags().BoolVar(&cfg.skipVersionCheck, "skip-version-check", false,
		"run even when CLI / daemon / connector versions disagree (audited escape hatch; also EASYEDA_SKIP_VERSION_CHECK=1)")
	// STALE_READ 机械门(internal/daemon/stalereads.go)的人工逃生口。**必须**是
	// 根级 persistent:那道门拦的是任何 pcb.* 读,而这类读散落在 pcb / board / view /
	// call / apply 多棵子命令树下 —— 挂在 `pcb` 一棵树上,拒绝消息对其余几棵就又成了
	// 一句做不到的承诺。名字刻意不叫 --force-reason:三个布线命令上已经有一个语义
	// 完全不同的 `--force <理由>`(阶段门),再来一个 --force-reason 会被读成「--force
	// 的理由」。照 --skip-version-check 的先例:逃生口用它拆的那道门命名。
	// 注意 usage 里的反引号:cobra 用**第一个**反引号词当值占位符,所以只能给
	// `reason`,不能顺手把 `easyeda doc reload` 引起来(否则 --help 显示成
	// `--force-stale-read easyeda doc reload`)。
	root.PersistentFlags().StringVar(&cfg.forceStaleRead, "force-stale-read", "",
		"read PCB state not reloaded since the last PCB edit: bypass the STALE_READ gate with this `reason` (audited as daemon.stale_read.force; only ever attaches to PCB reads, never unlocks the routing stage gate). Normal fix is: easyeda doc reload")
	root.PersistentFlags().StringVar(&cfg.doc, "doc", "",
		"pin every mutating action to this schematic page / PCB (uuid or name): the CLI switches to it and confirms via live document.current before editing, refusing rather than land the edit on whatever page is foreground — removes the doc-switch race")

	root.AddCommand(
		newVersionCmd(stdout),
		newActionsCmd(stdout, stderr),
		newNotifyCmd(cfg, stdout, stderr),
		newCallCmd(cfg, stdout, stderr),
		newApplyCmd(cfg, stdout, stderr),
		newDaemonCmd(cfg, stdout, stderr),
		newHealthAliasCmd(cfg, stdout, stderr),
		newAuditCmd(stdout, stderr),
		newProjectCmd(cfg, stdout, stderr),
		newDocCmd(cfg, stdout, stderr),
		newSchCmd(cfg, stdout, stderr),
		newPcbCmd(cfg, stdout, stderr),
		newWorkflowCmd(cfg, stdout, stderr),
		newSpecCmd(cfg, stdout, stderr),
		newBoardCmd(cfg, stdout, stderr),
		newViewCmd(cfg, stdout, stderr),
		newBomCmd(cfg, stdout, stderr),
		newManufacturingCmd(cfg, stdout, stderr),
		newLibCmd(cfg, stdout, stderr),
		newBlocksCmd(stdout, stderr),
		newApiCmd(stdout, stderr),
		newDebugCmd(cfg, stdout, stderr),
		newSkillCmd(stdout, stderr),
		newUpdateCmd(cfg, stdout, stderr),
	)

	return root
}

// ── version ───────────────────────────────────────────────────────────────

func newVersionCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(stdout, "%s %s\n", version.Name, version.Version)
			return nil
		},
	}
}

// ── notify ────────────────────────────────────────────────────────────────

func newNotifyCmd(cfg *appConfig, stdout, stderr io.Writer) *cobra.Command {
	var window, message, typ string
	var duration float64
	c := &cobra.Command{
		Use:   "notify",
		Short: "Show a toast inside the EasyEDA window (design-flow step notification)",
		Long: `Surface a non-blocking toast INSIDE the EasyEDA window. The design flow calls this
as each stage passes so the user can watch progress live — "完成 X,下一步 Y".
type ∈ info | success | warn | error | question.`,
		Args: cobra.NoArgs,
		Example: `  easyeda notify --message "完成 布局,下一步 布线" --type success
  easyeda notify --message "DRC 未通过,需修复" --type error --duration 5`,
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := map[string]any{"message": message}
			if typ != "" {
				payload["type"] = typ
			}
			if cmd.Flags().Changed("duration") {
				payload["duration"] = duration
			}
			return dispatch(cfg, "system.notify", window, payload, stdout, stderr)
		},
	}
	c.Flags().StringVar(&message, "message", "", "toast text (required)")
	c.Flags().StringVar(&typ, "type", "info", "info | success | warn | error | question")
	c.Flags().Float64Var(&duration, "duration", 3, "seconds to show")
	c.Flags().StringVar(&window, "window", "", "EasyEDA window ID (else use --project)")
	_ = c.MarkFlagRequired("message")
	return c
}

// ── actions ───────────────────────────────────────────────────────────────

func newActionsCmd(stdout, _ io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "actions",
		Short: "Print the typed action catalog as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(protocol.AllActions())
		},
	}
}

// ── call (generic escape hatch) ───────────────────────────────────────────

func newCallCmd(cfg *appConfig, stdout, stderr io.Writer) *cobra.Command {
	var window, payload string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "call <action>",
		Short: "Generic escape hatch: call any typed action directly",
		Args:  cobra.ExactArgs(1),
		Example: `  easyeda call system.health
  easyeda call schematic.components.list --window win-1
	  easyeda call pcb.drc.check --timeout 2m
  easyeda call schematic.component.place --payload '{"libraryUuid":"...","uuid":"...","x":100,"y":200}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			action := args[0]

			var payloadMap map[string]any
			if payload != "" {
				if err := json.Unmarshal([]byte(payload), &payloadMap); err != nil {
					return fmt.Errorf("invalid --payload json: %w", err)
				}
			}

			if timeout <= 0 {
				timeout = catalogActionTimeout(action)
			}
			return dispatchTimed(cfg, action, window, payloadMap, timeout, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&window, "window", "", "EasyEDA window ID")
	cmd.Flags().StringVar(&payload, "payload", "", "action payload as a JSON object")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "end-to-end timeout (default: action catalog timeoutMs; examples: 90s, 2m)")
	return cmd
}

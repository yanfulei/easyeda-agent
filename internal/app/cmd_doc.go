package app

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// openableDoc is one document a window can switch to (a schematic page or a PCB),
// unified across the schematic.pages.list and pcb.documents.list actions.
type openableDoc struct {
	UUID   string `json:"uuid"`
	Type   string `json:"type"` // "schematic" | "pcb"
	Name   string `json:"name"`
	Parent string `json:"parent,omitempty"` // owning schematic/project uuid
	Active bool   `json:"active"`
}

// newDocCmd returns the "doc" subcommand group — the self-service discover +
// switch loop. `doc ls` enumerates every openable document in the targeted
// window and marks the active one; `doc switch` resolves a name or uuid and
// brings that document to the front. Both route by the shared --project/--window
// flags, so an agent can drive a window without knowing its windowId or port.
func newDocCmd(cfg *appConfig, stdout, stderr io.Writer) *cobra.Command {
	var window string

	doc := &cobra.Command{
		Use:   "doc",
		Short: "Discover and open/switch EasyEDA documents (schematic pages / PCBs)",
		Long: "Discover every openable document in a window and open or switch between them.\n\n" +
			"  easyeda doc ls --project <name>                      list all schematic pages + PCBs, ★=active\n" +
			"  easyeda doc open <name|uuid> --project <name>        open a document (schematic page or PCB)\n" +
			"  easyeda doc switch <name|uuid> --project <name>      switch to a document (same as open)\n" +
			"  easyeda doc reload [name|uuid] --project <name>      save, close, and reopen a document\n\n" +
			"Context is read live (not the connect-time snapshot), so the active marker\nand `daemon health` reflect the real foreground document.",
	}
	doc.PersistentFlags().StringVar(&window, "window", "", "EasyEDA window ID (usually prefer --project)")

	var jsonOut bool

	lsCmd := &cobra.Command{
		Use:   "ls",
		Short: "List all openable documents in the window (★ = active)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			docs, active, _, err := discoverDocs(cfg, window)
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(stdout, map[string]any{
					"activeUuid": active,
					"documents":  docs,
				})
			}
			printDocTable(stdout, docs)
			return nil
		},
	}
	lsCmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of a table")

	switchCmd := &cobra.Command{
		Use:   "switch <name|uuid>",
		Short: "Switch the foreground document by page name, PCB name, or uuid",
		Args:  cobra.ExactArgs(1),
		Example: "  easyeda doc switch P2 --project motobox2026\n" +
			"  easyeda doc switch ESP32-S3-V1_0_1 --project motobox2026\n" +
			"  easyeda doc switch 6b3a2f01-... --project motobox2026",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			docs, _, win, err := discoverDocs(cfg, window)
			if err != nil {
				return err
			}
			match, err := resolveDoc(docs, target)
			if err != nil {
				return err
			}
			// Pin the open + readback to the SAME window discoverDocs resolved.
			if _, err := requestAction(cfg, "document.open", win,
				map[string]any{"uuid": match.UUID}); err != nil {
				return err
			}
			// Re-read live context to confirm the switch took effect.
			cur, err := requestAction(cfg, "document.current", win, nil)
			if err != nil {
				return err
			}
			// document.open returns as soon as the tab exists — BEFORE the page's
			// primitives/netlist finish loading. Wait for the page data to settle
			// so a read fired right after the switch doesn't sample a half-loaded
			// page (issue #67). The probe follows the document type — a PCB is
			// polled with pcb.components.list rather than skipped (issue #161).
			ready := waitDocSettleFor(cfg, win, match.Type)
			out := map[string]any{
				"switchedTo": match,
				"ready":      ready,
			}
			if cur.Context != nil {
				out["active"] = cur.Context
			}
			if jsonOut {
				return writeJSON(stdout, out)
			}
			fmt.Fprintf(stdout, "✓ switched to %s %q (%s)\n", match.Type, match.Name, match.UUID)
			if !ready {
				fmt.Fprintln(stdout, "⚠ page did not settle within the wait window — data may still be loading; re-read if results look empty")
			}
			return nil
		},
	}
	switchCmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of a line")

	// open is a semantically clearer alias for switch — "open a document" vs "switch to a document"
	openCmd := &cobra.Command{
		Use:   "open <name|uuid>",
		Short: "Open a document (schematic page or PCB) by name or uuid",
		Args:  cobra.ExactArgs(1),
		Example: "  easyeda doc open PCB1 --project ceshi\n" +
			"  easyeda doc open P1 --project ceshi\n" +
			"  easyeda doc open ESP32-mini-v2 --project hardware",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			docs, _, win, err := discoverDocs(cfg, window)
			if err != nil {
				return err
			}
			match, err := resolveDoc(docs, target)
			if err != nil {
				return err
			}
			if _, err := requestAction(cfg, "document.open", win,
				map[string]any{"uuid": match.UUID}); err != nil {
				return err
			}
			cur, err := requestAction(cfg, "document.current", win, nil)
			if err != nil {
				return err
			}
			out := map[string]any{
				"opened": match,
			}
			if cur.Context != nil {
				out["active"] = cur.Context
			}
			if jsonOut {
				return writeJSON(stdout, out)
			}
			fmt.Fprintf(stdout, "✓ opened %s %q (%s)\n", match.Type, match.Name, match.UUID)
			return nil
		},
	}
	openCmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of a line")

	// doc reload — save + close + reopen a document. Exists because some
	// per-document engine state only refreshes on a real close/reopen: a
	// freshly CREATED PCB's pour reflow keeps using a creation-time rules
	// snapshot — rule writes and pour-rebuilds are ignored until the document
	// is reloaded (tab-switching away and back does NOT reload). The esp32-mini
	// playbook relies on this after its pour sequence.
	reloadCmd := &cobra.Command{
		Use:   "reload [name|uuid]",
		Short: "Save + close + reopen a document (default: the active one) — refreshes per-doc engine state",
		Long: `Save a document, close its tab, and reopen it — a real reload, unlike
"doc switch" which only changes the foreground tab.

Why: a freshly CREATED PCB document's copper-pour reflow keeps using the rules
snapshot taken at creation — writing rules (pcb drc-rules-set) and re-pouring
(pcb pour-rebuild) have NO effect until the document is closed and reopened.
After a reload the reflow honors the current rule configuration (clearance AND
thermal-spoke generation). Run "pcb pour-rebuild" after reloading a PCB.

The target document is saved first (schematic.save / pcb.save by type), so no
edits are lost. Defaults to the active document; pass a name/uuid to reload
another (it is brought to the front first).

The reopen leaves the reloaded document foreground, so reloading a NON-active
page would move the active tab. This command restores the pre-reload active
document afterward and reports it as "activeRestored", so the ★ does not drift
(issue #67).`,
		Args: cobra.MaximumNArgs(1),
		Example: `  easyeda doc reload                      # reload the active document
  easyeda doc reload PCB3 --project ceshi # reload a specific PCB
  easyeda pcb pour-rebuild                # then re-pour under the refreshed rules`,
		RunE: func(cmd *cobra.Command, args []string) error {
			docs, activeUUID, win, err := discoverDocs(cfg, window)
			if err != nil {
				return err
			}
			target := activeUUID
			if len(args) == 1 {
				match, err := resolveDoc(docs, args[0])
				if err != nil {
					return err
				}
				target = match.UUID
			}
			if target == "" {
				return fmt.Errorf("no active document to reload (run `easyeda doc ls`)")
			}
			docType, err := reloadDocumentByUUID(cfg, win, target)
			if err != nil {
				return err
			}
			// Restore the pre-reload active document. reopen leaves the target
			// foreground even when the caller reloaded a NON-active page, so
			// without this the ★ silently drifts and later commands land on the
			// wrong page (issue #67). Best-effort: a restore failure is reported
			// but the reload itself already succeeded.
			restored := activeUUID
			if activeUUID != "" && activeUUID != target {
				if _, rerr := requestAction(cfg, "document.open", win,
					map[string]any{"uuid": activeUUID}); rerr != nil {
					restored = ""
				}
			}
			out := map[string]any{
				"reloaded":       target,
				"documentType":   docType,
				"saved":          true,
				"activeRestored": restored,
			}
			if jsonOut {
				return writeJSON(stdout, out)
			}
			fmt.Fprintf(stdout, "✓ reloaded %s %s (saved → closed → reopened)\n", docType, target)
			if activeUUID != "" && activeUUID != target {
				if restored != "" {
					fmt.Fprintf(stdout, "  ↩ restored active document to %s\n", restored)
				} else {
					fmt.Fprintf(stdout, "  ⚠ could not restore active document %s — active is now %s\n", activeUUID, target)
				}
			}
			return nil
		},
	}
	reloadCmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of a line")

	doc.AddCommand(lsCmd, switchCmd, openCmd, reloadCmd)
	return doc
}

// discoverDocs resolves the target window ONCE, then aggregates
// schematic.pages.list + pcb.documents.list into a single openable-document list
// and marks the one matching the live active document (document.current). Every
// sub-call is pinned to the resolved windowId, so a second window appearing or a
// single-window auto-target racing mid-command can't break it. Returns the
// resolved windowId so a caller (e.g. `doc switch`) can pin its own follow-ups.
// A project with no PCB returns a successful empty pcbs array. A listing error
// is not equivalent: hiding it makes an active PCB disappear from `doc ls` and
// turns the actionable error into a misleading "no document named" failure.
func discoverDocs(cfg *appConfig, window string) (docs []openableDoc, activeUUID, resolvedWindow string, err error) {
	docs, activeUUID, resolvedWindow, _, err = discoverDocsWithContext(cfg, window)
	return docs, activeUUID, resolvedWindow, err
}

// discoverDocsWithContext is discoverDocs plus the live context sampled by its
// document.current call. The --doc dispatch guard consumes that same sample to
// bind the final request without inserting another action between confirmation
// and dispatch.
func discoverDocsWithContext(cfg *appConfig, window string) (docs []openableDoc, activeUUID, resolvedWindow string, current *actionContext, err error) {
	resolvedWindow, err = resolveTargetWindow(cfg, window)
	if err != nil {
		return nil, "", "", nil, err
	}

	cur, err := requestAction(cfg, "document.current", resolvedWindow, nil)
	if err != nil {
		return nil, "", "", nil, err
	}
	current = cur.Context
	if cur.Context != nil {
		activeUUID = cur.Context.DocumentUUID
	}

	pages, err := requestAction(cfg, "schematic.pages.list", resolvedWindow, nil)
	if err != nil {
		return nil, "", "", nil, err
	}
	for _, p := range mapsField(pages.Result, "pages") {
		docs = append(docs, openableDoc{
			UUID:   strField(p, "uuid"),
			Type:   "schematic",
			Name:   strField(p, "name"),
			Parent: strField(p, "parentSchematicUuid"),
		})
	}

	// PCBs are optional, but the action itself is not: a schematic-only project
	// returns {pcbs:[]}. Propagate transport/gate/SDK errors instead of silently
	// converting them to an empty inventory.
	pcbs, err := requestAction(cfg, "pcb.documents.list", resolvedWindow, nil)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("list PCB documents: %w", err)
	}
	for _, p := range mapsField(pcbs.Result, "pcbs") {
		docs = append(docs, openableDoc{
			UUID:   strField(p, "uuid"),
			Type:   "pcb",
			Name:   strField(p, "name"),
			Parent: strField(p, "parentProjectUuid"),
		})
	}

	for i := range docs {
		if docs[i].UUID != "" && docs[i].UUID == activeUUID {
			docs[i].Active = true
		}
	}
	sort.SliceStable(docs, func(i, j int) bool {
		if docs[i].Type != docs[j].Type {
			return docs[i].Type < docs[j].Type
		}
		return docs[i].Name < docs[j].Name
	})
	return docs, activeUUID, resolvedWindow, current, nil
}

// resolveDoc maps a user-supplied name or uuid to exactly one openable doc.
// An exact uuid match wins; otherwise a case-insensitive name match is used.
// Ambiguous name matches return an error listing the candidates.
func resolveDoc(docs []openableDoc, target string) (openableDoc, error) {
	for _, d := range docs {
		if d.UUID == target {
			return d, nil
		}
	}
	var hits []openableDoc
	lt := strings.ToLower(target)
	for _, d := range docs {
		if strings.ToLower(d.Name) == lt {
			hits = append(hits, d)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return openableDoc{}, fmt.Errorf("no document named or with uuid %q (run `easyeda doc ls` to see options)", target)
	default:
		var names []string
		for _, h := range hits {
			names = append(names, fmt.Sprintf("%s/%s", h.Type, h.UUID))
		}
		return openableDoc{}, fmt.Errorf("%q is ambiguous: %s — pass a uuid", target, strings.Join(names, ", "))
	}
}

func printDocTable(w io.Writer, docs []openableDoc) {
	if len(docs) == 0 {
		fmt.Fprintln(w, "(no openable documents — is a project open in this window?)")
		return
	}
	fmt.Fprintf(w, "%-2s  %-9s  %-24s  %s\n", "", "TYPE", "NAME", "UUID")
	for _, d := range docs {
		marker := " "
		if d.Active {
			marker = "★"
		}
		fmt.Fprintf(w, "%-2s  %-9s  %-24s  %s\n", marker, d.Type, d.Name, d.UUID)
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// mapsField returns result[key] as a slice of string-keyed maps, tolerating the
// any-typed shape that survives JSON round-tripping.
func mapsField(result map[string]any, key string) []map[string]any {
	if result == nil {
		return nil
	}
	raw, ok := result[key].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func strField(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// reloadDocumentByUUID saves + closes + reopens document `target` in window
// `win` and waits for it to become the active document again — the extracted
// core of `doc reload`, shared with `pcb clear`'s verify pass (#121: some
// primitives are only enumerable after a real close/reopen, so a clear must
// reload before it can trust "the board is empty"). Brings the target to the
// foreground first when it isn't already. Returns the document's type
// ("pcb"/"schematic") so callers can branch.
func reloadDocumentByUUID(cfg *appConfig, win, target string) (string, error) {
	return reloadDocumentByUUIDWithRuntime(cfg, win, target, reloadDocumentRuntime{
		request: requestAction,
		settle:  waitDocSettleFor,
		sleep:   time.Sleep,
		now:     time.Now,
	})
}

type reloadDocumentRuntime struct {
	request func(*appConfig, string, string, any) (*actionResult, error)
	settle  func(*appConfig, string, string) bool
	sleep   func(time.Duration)
	now     func() time.Time
}

func reloadDocumentByUUIDWithRuntime(cfg *appConfig, win, target string, rt reloadDocumentRuntime) (string, error) {
	cur, err := rt.request(cfg, "document.current", win, nil)
	if err != nil {
		return "", err
	}
	if cur.Context == nil || cur.Context.DocumentUUID != target || cur.Context.TabID == "" {
		if _, err := rt.request(cfg, "document.open", win, map[string]any{"uuid": target}); err != nil {
			return "", err
		}
		cur, err = rt.request(cfg, "document.current", win, nil)
		if err != nil {
			return "", err
		}
		if cur.Context == nil || cur.Context.DocumentUUID != target || cur.Context.TabID == "" {
			return "", fmt.Errorf("could not activate document %s before reload (active=%v)", target, cur.Context)
		}
	}
	docType := cur.Context.DocumentType
	saveAction := "schematic.save"
	if docType == "pcb" {
		saveAction = "pcb.save"
	}
	saved, err := rt.request(cfg, saveAction, win, nil)
	if err != nil {
		return docType, fmt.Errorf("save before reload failed: %w", err)
	}
	if err := requireConfirmedSave(saveAction, saved); err != nil {
		return docType, fmt.Errorf("save before reload was not confirmed: %w", err)
	}
	closed, err := rt.request(cfg, "document.close", win, map[string]any{"tabId": cur.Context.TabID})
	if err != nil {
		return docType, fmt.Errorf("close document failed: %w", err)
	}
	if err := requireConfirmedClose(closed); err != nil {
		return docType, fmt.Errorf("close document was not confirmed: %w", err)
	}
	rt.sleep(1 * time.Second)
	if _, err := rt.request(cfg, "document.open", win, map[string]any{"uuid": target}); err != nil {
		return docType, fmt.Errorf("reopen after close failed: %w", err)
	}
	// Poll until the reopened document is the live active one.
	deadline := rt.now().Add(10 * time.Second)
	for {
		cur, err = rt.request(cfg, "document.current", win, nil)
		if err == nil && cur.Context != nil && cur.Context.DocumentUUID == target {
			if !rt.settle(cfg, win, docType) {
				return docType, fmt.Errorf("document %s reopened but its %s inventory did not settle", target, docType)
			}
			return docType, nil
		}
		if rt.now().After(deadline) {
			return docType, fmt.Errorf("document %s did not become active within 10s after reopen", target)
		}
		rt.sleep(500 * time.Millisecond)
	}
}

// requireConfirmedSave is the destructive boundary for doc reload. An ok:true
// envelope is insufficient: older connectors returned ok:true even when the SDK
// save call returned false. Closing the tab is allowed only with saved:true.
func requireConfirmedSave(action string, res *actionResult) error {
	if res == nil || res.Result == nil {
		return fmt.Errorf("%s returned no save result", action)
	}
	saved, ok := res.Result["saved"].(bool)
	if !ok || !saved {
		return fmt.Errorf("%s did not return saved:true", action)
	}
	return nil
}

func requireConfirmedClose(res *actionResult) error {
	if res == nil || res.Result == nil {
		return fmt.Errorf("document.close returned no result")
	}
	closed, ok := res.Result["closed"].(bool)
	if !ok || !closed {
		return fmt.Errorf("document.close did not return closed:true")
	}
	return nil
}

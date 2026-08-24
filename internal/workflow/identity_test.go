package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// identity_test.go — 同名重建的身份判定。所有用例都自带数据,不碰共享 helper。

func newBoundState(t *testing.T, uuid string) *State {
	t.Helper()
	s := &State{Project: "ceshi", Confirmed: map[Stage]bool{}}
	s.Bind(uuid)
	return s
}

// 页写入在绑定后自动盖戳;未绑定(离线)时不盖 —— 宁可留「未知」也不猜。
func TestStampOnPageWrite(t *testing.T) {
	s := newBoundState(t, "uuidA")
	s.SetGroupsForPage("pageA", []*Group{{ID: "g1", Members: []string{"R1"}}})
	if got := s.PageOwnerOf("pageA"); got != "uuidA" {
		t.Fatalf("bound write should stamp owner, got %q", got)
	}

	offline := &State{Project: "ceshi", Confirmed: map[Stage]bool{}}
	offline.SetGroupsForPage("pageX", []*Group{{ID: "g1", Members: []string{"R1"}}})
	if got := offline.PageOwnerOf("pageX"); got != "" {
		t.Fatalf("unbound write must not invent an owner, got %q", got)
	}
	// 未知归属默认参与跨页读取:证不出外来就不误伤(旧格式文件的不变式)。
	if !offline.PageInScope("pageX", "uuidA") {
		t.Fatal("unstamped page must stay in scope")
	}
}

// 清空一页的组表时,若没有别的表还钉着这一页,归属戳跟着走。
func TestOwnerDroppedWithLastTable(t *testing.T) {
	s := newBoundState(t, "uuidA")
	s.SetGroupsForPage("pageA", []*Group{{ID: "g1", Members: []string{"R1"}}})
	s.SetGroupsForPage("pageA", nil)
	if got := s.PageOwnerOf("pageA"); got != "" {
		t.Fatalf("owner should be dropped with the last table, got %q", got)
	}

	s2 := newBoundState(t, "uuidA")
	s2.SetGroupsForPage("pageA", []*Group{{ID: "g1", Members: []string{"R1"}}})
	s2.SetSchZonesForPage("pageA", map[string]*SchZoneClaim{"m": {Zone: "left", Parts: []string{"R1"}}})
	s2.SetGroupsForPage("pageA", nil)
	if got := s2.PageOwnerOf("pageA"); got != "uuidA" {
		t.Fatalf("owner must survive while another table still keys the page, got %q", got)
	}
}

// 同名重建:文件此前绑过 uuidA,现在窗口是 uuidB。此刻还没戳的页只能是 A 写的,
// 归给 A —— 这一步让「只在绑定那一刻可见」的事实变成数据,死页当场退出跨页匹配。
func TestBindRebuildAttributesUnstampedPagesToPriorProject(t *testing.T) {
	s := &State{
		Project: "ceshi", ProjectUUID: "uuidA", Confirmed: map[Stage]bool{},
		GroupsByPage: map[string][]*Group{
			"deadPage": {{ID: "g1", Members: []string{"R1"}}},
			"livePage": {{ID: "g2", Members: []string{"R2"}}},
		},
	}
	res := s.Bind("uuidB")
	if !res.Rebuilt {
		t.Fatal("prior uuid != live uuid must be reported as a rebuild")
	}
	if len(res.Foreign) != 2 {
		t.Fatalf("both unstamped pages should be attributed to the prior project, got %v", res.Foreign)
	}
	if s.PageOwnerOf("deadPage") != "uuidA" {
		t.Fatalf("attribution not persisted: %q", s.PageOwnerOf("deadPage"))
	}
	if s.PageInScope("deadPage", "uuidB") {
		t.Fatal("a page proven to belong to the previous project must leave the cross-page scope")
	}
	if s.ProjectUUID != "uuidB" {
		t.Fatalf("ProjectUUID should follow the live window, got %q", s.ProjectUUID)
	}
	if msg := res.Message("ceshi"); msg == "" {
		t.Fatal("a rebuild must produce a human message")
	}
	// 重建后新写的页盖的是新 uuid,照常参与。
	s.SetGroupsForPage("livePage", []*Group{{ID: "g2", Members: []string{"R2"}}})
	if !s.PageInScope("livePage", "uuidB") {
		t.Fatal("a page re-written under the live project must be in scope")
	}
}

// 旧格式(从未绑过 uuid)**不做归因** —— 没有任何证据说明那些页是谁的。
func TestBindLegacyDoesNotAttribute(t *testing.T) {
	s := &State{
		Project: "ceshi", Confirmed: map[Stage]bool{},
		GroupsByPage: map[string][]*Group{"p1": {{ID: "g1", Members: []string{"R1"}}}},
	}
	res := s.Bind("uuidB")
	if res.Rebuilt {
		t.Fatal("a first bind is not a rebuild")
	}
	if !res.LegacyUnstamped || len(res.Unowned) != 1 {
		t.Fatalf("legacy file should be flagged as unstamped: %+v", res)
	}
	if s.PageOwnerOf("p1") != "" {
		t.Fatal("legacy pages must NOT be attributed on a first bind (no evidence)")
	}
	if !s.PageInScope("p1", "uuidB") {
		t.Fatal("unproven pages stay in scope")
	}
	if msg := res.Message("ceshi"); msg == "" {
		t.Fatal("a legacy unstamped file must produce a human message")
	}
}

// 活体页表核销:在表里的盖戳,不在的标外来。空表 / 无 uuid 一律拒绝 ——
// 读失败也是零页,拿它当证据就等于用一次抖动毁掉整份记账。
func TestAdoptLivePages(t *testing.T) {
	mk := func() *State {
		return &State{
			Project: "ceshi", Confirmed: map[Stage]bool{},
			GroupsByPage: map[string][]*Group{
				"live1": {{ID: "g1", Members: []string{"R1"}}},
				"dead1": {{ID: "g2", Members: []string{"R2"}}},
				"dead2": {{ID: "g3", Members: []string{"R3"}}},
			},
		}
	}

	if _, _, refused := mk().AdoptLivePages("uuidB", nil); !refused {
		t.Fatal("an empty live page list must be refused, not treated as proof")
	}
	if _, _, refused := mk().AdoptLivePages("", []string{"live1"}); !refused {
		t.Fatal("reaping without a live project uuid must be refused")
	}

	s := mk()
	stamped, foreign, refused := s.AdoptLivePages("uuidB", []string{"live1"})
	if refused {
		t.Fatal("a non-empty live page list must be accepted")
	}
	if !reflect.DeepEqual(stamped, []string{"live1"}) {
		t.Fatalf("stamped = %v", stamped)
	}
	if !reflect.DeepEqual(foreign, []string{"dead1", "dead2"}) {
		t.Fatalf("foreign = %v", foreign)
	}
	if s.PageOwnerOf("dead1") != ForeignOwner {
		t.Fatalf("dead page owner = %q", s.PageOwnerOf("dead1"))
	}
	if s.PageInScope("dead1", "uuidB") || !s.PageInScope("live1", "uuidB") {
		t.Fatal("reap must narrow the cross-page scope to the live pages")
	}
	// 核销**不删数据**。
	if len(s.GroupsByPage) != 3 {
		t.Fatalf("reap must not delete anything, pages left = %d", len(s.GroupsByPage))
	}
}

// 删除只走显式 PrunePages,且只碰点名的页。
func TestPrunePagesOnlyTouchesNamedPages(t *testing.T) {
	s := &State{
		Project: "ceshi", Confirmed: map[Stage]bool{},
		GroupsByPage:          map[string][]*Group{"dead": {{ID: "g1"}}, "live": {{ID: "g2"}}},
		SchZonesByPage:        map[string]map[string]*SchZoneClaim{"dead": {"m": {Zone: "left"}}},
		SchZoneFrameIdsByPage: map[string]*SchZoneFrames{"dead": {Rects: []string{"r1"}}},
		PageOwners:            map[string]string{"dead": ForeignOwner, "live": "uuidB"},
	}
	removed := s.PrunePages([]string{"dead"})
	if !reflect.DeepEqual(removed, []string{"dead"}) {
		t.Fatalf("removed = %v", removed)
	}
	if _, ok := s.GroupsByPage["dead"]; ok {
		t.Fatal("dead page groups survived prune")
	}
	if _, ok := s.SchZonesByPage["dead"]; ok {
		t.Fatal("dead page zone claims survived prune")
	}
	if _, ok := s.SchZoneFrameIdsByPage["dead"]; ok {
		t.Fatal("dead page frames survived prune")
	}
	if s.PageOwnerOf("dead") != "" {
		t.Fatal("dead page owner stamp survived prune")
	}
	if len(s.GroupsByPage["live"]) != 1 || s.PageOwnerOf("live") != "uuidB" {
		t.Fatal("prune must not touch pages it was not given")
	}
}

// 迁移:旧格式文件(无 projectUuid / pageOwners)必须原样读得出来,新字段
// 只在真的有值时才落盘 —— 否则每份没绑过的状态都会平白多出两个键。
func TestLegacyFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	legacy := `{
  "project": "ceshi",
  "confirmed": {"imported": true},
  "groupsByPage": {"p1": [{"id": "g1", "members": ["R1"]}]},
  "updatedAt": "2026-08-20T10:00:00+08:00"
}`
	if err := os.WriteFile(filepath.Join(dir, "ceshi.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Load("ceshi")
	if err != nil {
		t.Fatalf("legacy state must still load: %v", err)
	}
	if len(st.GroupsByPage["p1"]) != 1 || !st.Has(StageImported) {
		t.Fatal("legacy content lost on load")
	}
	if st.ProjectUUID != "" || st.PageOwners != nil {
		t.Fatal("legacy file must not gain a fabricated identity on load")
	}
	if err := Save(st); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(Path("ceshi"))
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["projectUuid"]; ok {
		t.Fatal("projectUuid must be omitted when unknown")
	}
	if _, ok := probe["pageOwners"]; ok {
		t.Fatal("pageOwners must be omitted when empty")
	}

	// 绑定 + 写一页之后,新字段落盘并读得回来。
	st.Bind("uuidB")
	st.SetGroupsForPage("p2", []*Group{{ID: "g2", Members: []string{"R2"}}})
	if err := Save(st); err != nil {
		t.Fatal(err)
	}
	back, err := Load("ceshi")
	if err != nil {
		t.Fatal(err)
	}
	if back.ProjectUUID != "uuidB" || back.PageOwners["p2"] != "uuidB" {
		t.Fatalf("identity did not round-trip: uuid=%q owners=%v", back.ProjectUUID, back.PageOwners)
	}
	// bound 不落盘:它是「本进程这个窗口」的事实,不是文件的事实。
	if back.BoundUUID() != "" {
		t.Fatal("bound uuid must not be persisted")
	}
}

// PageRecords 是 `workflow pages` 的展示口径:三档判定必须与 PageInScope 一致
// (判定与展示同一把尺 —— 两把尺是这个仓库反复付过学费的病)。
func TestPageRecordsAgreeWithScope(t *testing.T) {
	s := &State{
		Project: "ceshi", Confirmed: map[Stage]bool{},
		GroupsByPage: map[string][]*Group{"own": {{ID: "g1"}}, "foreign": {{ID: "g2"}}, "unknown": {{ID: "g3"}}},
		PageOwners:   map[string]string{"own": "uuidB", "foreign": "uuidA"},
	}
	for _, r := range s.PageRecords("uuidB", []string{"own"}) {
		inScope := s.PageInScope(r.UUID, "uuidB")
		if (r.Verdict == "foreign") == inScope {
			t.Fatalf("page %s: verdict %q disagrees with PageInScope=%v", r.UUID, r.Verdict, inScope)
		}
		if r.UUID == "own" && !r.Live {
			t.Fatal("live flag lost")
		}
	}
}

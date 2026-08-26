package app

import "testing"

// 写图签只传**用户点名的项**(2026-08-26 起不再整包回传)。
//
// 曾经的 TestTbKeepStructural 测的是「整包回传时按住 Title Block/Border 两个结构
// 开关」——那套保护随整包回传一起删了:整包回传本身才是图框损毁的成因(#186)。
// 现在的不变式是**结构键压根不进 payload**,由下面这条钉住。
func TestSchTitleBlockMerge_OnlySendsRequestedKeys(t *testing.T) {
	full := map[string]any{
		"Title Block": map[string]any{"value": "1"},
		"Border":      map[string]any{"value": "1"},
		"Device":      map[string]any{"value": "Drawing-Symbol_A4"},
		"Name":        map[string]any{"value": "old"},
		"Drawed":      map[string]any{"value": ""},
	}
	out := map[string]any{}
	for k, v := range map[string]any{"Name": "new"} {
		if _, ok := full[k]; !ok {
			t.Fatalf("fixture 里应有 %s", k)
		}
		out[k] = map[string]any{"showTitle": true, "showValue": true, "value": v}
	}
	if len(out) != 1 {
		t.Fatalf("只该下发被点名的 1 项,实际 %d 项: %v", len(out), out)
	}
	for _, forbidden := range []string{"Title Block", "Border", "Device"} {
		if _, present := out[forbidden]; present {
			t.Errorf("结构键 %s 绝不能出现在下发数据里(#186 图框损毁成因)", forbidden)
		}
	}
}


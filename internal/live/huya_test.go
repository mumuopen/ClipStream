package live

// 虎牙房间路径解析：数字与短别名兼容（浏览器会把数字房间重定向为别名，如 /lpl）

import "testing"

func TestHuyaRoomPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"数字房间", "https://www.huya.com/660000", "660000"},
		{"短别名", "https://www.huya.com/lpl", "lpl"},
		{"m 端数字", "http://m.huya.com/441195", "441195"},
		{"带 query", "https://www.huya.com/660000?share=1", "660000"},
		{"带锚点", "https://www.huya.com/lpl#comment", "lpl"},
		{"带二级路径", "https://www.huya.com/lpl/abc", "lpl"},
		{"无 huya 域", "https://live.bilibili.com/7734200", ""},
		{"空串", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HuyaRoomPath(tc.in); got != tc.want {
				t.Fatalf("HuyaRoomPath(%q)=%q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
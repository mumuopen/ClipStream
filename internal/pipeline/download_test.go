package pipeline

// download 阶段直播间防呆识别（直播走 daemon 录制，yt-dlp 只搬离线视频）

import "testing"

func TestIsLiveRoomURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"虎牙直播", "https://www.huya.com/660000", true},
		{"虎牙直播-无协议头", "www.huya.com/660000", true},
		{"B站直播间", "live.bilibili.com/7734200", true},
		{"抖音直播间", "https://live.douyin.com/12345", true},
		{"B站稿件(搬运走yt-dlp)", "https://www.bilibili.com/video/BV1GJ411x7h7", false},
		{"YouTube视频(搬运走yt-dlp)", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", false},
		{"抖音短视频(搬运走yt-dlp)", "https://www.douyin.com/video/12345", false},
		{"虎牙非数字路径(忽略)", "https://www.huya.com/g/1060", false},
		{"本地文件(非URL)", "C:\\video\\test.mp4", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLiveRoomURL(tc.in); got != tc.want {
				t.Fatalf("isLiveRoomURL(%q)=%v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
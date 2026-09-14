package handlers

import "testing"

func TestResolutionWH(t *testing.T) {
	widthTests := []struct {
		name  string
		input string
		want  int
	}{
		{name: "WxH", input: "1920x1080", want: 1920},
		{name: "label", input: "1080p", want: 1920},
		{name: "junk rejected", input: "1920x1080garbage", want: 0},
	}
	for _, tt := range widthTests {
		t.Run("width/"+tt.name, func(t *testing.T) {
			if got := resolutionWidth(tt.input); got != tt.want {
				t.Fatalf("resolutionWidth(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}

	heightTests := []struct {
		name  string
		input string
		want  int
	}{
		{name: "WxH", input: "1920x1080", want: 1080},
		{name: "label", input: "1080p", want: 1080},
	}
	for _, tt := range heightTests {
		t.Run("height/"+tt.name, func(t *testing.T) {
			if got := resolutionHeight(tt.input); got != tt.want {
				t.Fatalf("resolutionHeight(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

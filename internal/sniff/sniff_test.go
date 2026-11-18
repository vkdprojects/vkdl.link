package sniff

import "testing"

func TestDetect(t *testing.T) {
	cases := []struct {
		name string
		head string
		want Kind
		ok   bool
	}{
		{"png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR", PNG, true},
		{"jpeg", "\xff\xd8\xff\xe0\x00\x10JFIF", JPEG, true},
		{"gif87", "GIF87a\x01\x00", GIF, true},
		{"gif89", "GIF89a\x01\x00", GIF, true},
		{"webp", "RIFF\x24\x00\x00\x00WEBPVP8 ", WebP, true},
		{"svg", `<svg xmlns="http://www.w3.org/2000/svg"></svg>`, Kind{}, false},
		{"svg-xml", `<?xml version="1.0"?><svg/>`, Kind{}, false},
		{"text", "hello world", Kind{}, false},
		{"wav", "RIFF\x24\x00\x00\x00WAVEfmt ", Kind{}, false},
		{"short-riff", "RIFF", Kind{}, false},
		{"png-truncated", "\x89PNG", Kind{}, false},
		{"empty", "", Kind{}, false},
		{"html", "<!doctype html><script>", Kind{}, false},
	}
	for _, c := range cases {
		got, ok := Detect([]byte(c.head))
		if ok != c.ok || got != c.want {
			t.Errorf("%s: Detect = %v, %v; want %v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestByExt(t *testing.T) {
	if k, ok := ByExt("jpg"); !ok || k != JPEG {
		t.Errorf("ByExt(jpg) = %v, %v", k, ok)
	}
	if _, ok := ByExt("svg"); ok {
		t.Error("svg não pode ser aceito")
	}
}

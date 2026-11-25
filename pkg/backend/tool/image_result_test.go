package tool

import (
	"strings"
	"testing"
)

func TestImageResultContent_DecodesDescriptionAndImage(t *testing.T) {
	content, err := ImageResultContent(`{"description":"schema.png","blocks":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(content) != 2 || content[0].Type != "text" || content[0].Text != "schema.png" {
		t.Fatalf("content = %+v, want the description then the image", content)
	}
	img := content[1]
	if img.Type != "image" || img.Source == nil || img.Source.Data != "QUJD" || img.Source.MediaType != "image/png" {
		t.Fatalf("image block = %+v", img)
	}
}

func TestImageResultContent_AcceptsADescriptionAlone(t *testing.T) {
	content, err := ImageResultContent(`{"description":"left_click at (1, 2)"}`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(content) != 1 || content[0].Text != "left_click at (1, 2)" {
		t.Fatalf("content = %+v", content)
	}
}

func TestImageResultContent_RefusesWhatItCannotCarry(t *testing.T) {
	for name, output := range map[string]string{
		"not json":         `iVBORw0KGgo`,
		"empty":            `{}`,
		"unknown block":    `{"description":"x","blocks":[{"type":"audio"}]}`,
		"image, no source": `{"description":"x","blocks":[{"type":"image"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ImageResultContent(output); err == nil {
				t.Errorf("ImageResultContent(%q) = nil error", output)
			}
		})
	}
}

func TestImageResultTrait_OnlyTheImageToolsCarryIt(t *testing.T) {
	want := map[string]bool{"read_image": true, "screenshot": true, "computer_use": true}
	images := NewRegistry()
	if err := RegisterClawReadImage(images, NewReadScope(t.TempDir())); err != nil {
		t.Fatalf("register read_image: %v", err)
	}
	if err := RegisterClawComputerUse(images); err != nil {
		t.Fatalf("register computer use: %v", err)
	}
	builtins := NewRegistry()
	if err := RegisterClawBuiltins(builtins, t.TempDir()); err != nil {
		t.Fatalf("register builtins: %v", err)
	}
	seen := 0
	for _, reg := range []*Registry{images, builtins} {
		for _, td := range reg.List() {
			seen++
			if got := td.ToDelegateDef().ImageResult; got != want[td.QualifiedName] {
				t.Errorf("%s: ImageResult = %t, want %t", td.QualifiedName, got, want[td.QualifiedName])
			}
		}
	}
	if seen < 6 {
		t.Fatalf("only %d tools registered — the check saw too little to mean anything", seen)
	}
}

// One image over the per-image cap a vision request accepts is refused, so
// the model reads an error it recovers from instead of a 400 that poisons
// the conversation; an image at the cap passes.
func TestImageResultContent_RefusesAnImageOverThePerImageCap(t *testing.T) {
	result := func(n int) string {
		return `{"description":"big.png","blocks":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + strings.Repeat("A", n) + `"}}]}`
	}
	if _, err := ImageResultContent(result(MaxImageBase64Bytes)); err != nil {
		t.Fatalf("an image at the cap was refused: %v", err)
	}
	_, err := ImageResultContent(result(MaxImageBase64Bytes + 1))
	if err == nil || !strings.Contains(err.Error(), "downscale") {
		t.Fatalf("err = %v, want a refusal telling the model to downscale", err)
	}
}

package imageutil

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// makeJPEG encodes a solid-color RGBA image of the given size as JPEG bytes.
func makeJPEG(w, h int, c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	return buf.Bytes()
}

func TestAssembleSingleStrip(t *testing.T) {
	raw := makeJPEG(100, 32, color.RGBA{255, 0, 0, 255})
	out, err := AssembleStrips(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Single strip: fast path returns raw bytes unchanged
	if !bytes.Equal(out, raw) {
		t.Errorf("single-strip fast path: output differs from input (want identity)")
	}
}

func TestAssembleTwoStrips(t *testing.T) {
	strip1 := makeJPEG(100, 32, color.RGBA{255, 0, 0, 255})
	strip2 := makeJPEG(100, 32, color.RGBA{0, 0, 255, 255})
	raw := append(strip1, strip2...)

	out, err := AssembleStrips(raw)
	if err != nil {
		t.Fatal(err)
	}

	// Decode result and check dimensions
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode assembled image: %v", err)
	}
	if img.Bounds().Dx() != 100 {
		t.Errorf("width: want 100, got %d", img.Bounds().Dx())
	}
	if img.Bounds().Dy() != 64 {
		t.Errorf("height: want 64 (32+32), got %d", img.Bounds().Dy())
	}
}

func TestSOIDetection(t *testing.T) {
	strip1 := makeJPEG(80, 16, color.RGBA{100, 100, 100, 255})
	strip2 := makeJPEG(80, 16, color.RGBA{200, 200, 200, 255})
	raw := append(strip1, strip2...) // concatenated, no padding

	offsets := findSOI(raw)
	if len(offsets) != 2 {
		t.Errorf("want 2 SOI markers, found %d", len(offsets))
	}
}

func TestPagesToPDFSinglePage(t *testing.T) {
	page := makeJPEG(100, 141, color.RGBA{0, 200, 0, 255}) // ~A4 aspect
	pdfBytes, err := PagesToPDF([][]byte{page})
	if err != nil {
		t.Fatal(err)
	}
	if len(pdfBytes) < 10 {
		t.Fatal("PDF output too small")
	}
	if !bytes.HasPrefix(pdfBytes, []byte("%PDF-")) {
		t.Errorf("output does not start with %%PDF- header")
	}
}

func TestPagesToPDFTwoPages(t *testing.T) {
	page1 := makeJPEG(100, 141, color.RGBA{0, 200, 0, 255})
	page2 := makeJPEG(100, 141, color.RGBA{0, 0, 200, 255})
	twoPage, err := PagesToPDF([][]byte{page1, page2})
	if err != nil {
		t.Fatal(err)
	}
	onePage, _ := PagesToPDF([][]byte{page1})
	if len(twoPage) <= len(onePage) {
		t.Errorf("two-page PDF (%d bytes) should be larger than one-page (%d bytes)", len(twoPage), len(onePage))
	}
}

func TestPageToJPEG(t *testing.T) {
	raw := makeJPEG(50, 50, color.RGBA{128, 128, 128, 255})
	out := PageToJPEG(raw)
	if !bytes.Equal(out, raw) {
		t.Error("PageToJPEG should return input unchanged")
	}
}

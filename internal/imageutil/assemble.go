package imageutil

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"

	"github.com/go-pdf/fpdf"
)

// AssembleStrips reassembles Samsung JPEG scan strips into a single JPEG image.
// The printer delivers one page as N concatenated JPEG strips (each 32 scan lines).
// Decoding the raw bytes gives only the first strip; this function composites them.
func AssembleStrips(raw []byte) ([]byte, error) {
	offsets := findSOI(raw)
	if len(offsets) <= 1 {
		return raw, nil // single strip — fast path
	}

	strips := splitStrips(raw, offsets)
	images := make([]image.Image, len(strips))
	totalH := 0
	for i, s := range strips {
		img, err := jpeg.Decode(bytes.NewReader(s))
		if err != nil {
			return nil, fmt.Errorf("decode strip %d: %w", i, err)
		}
		images[i] = img
		totalH += img.Bounds().Dy()
	}

	width := images[0].Bounds().Dx()
	canvas := image.NewRGBA(image.Rect(0, 0, width, totalH))
	y := 0
	for _, img := range images {
		h := img.Bounds().Dy()
		draw.Draw(canvas, image.Rect(0, y, width, y+h), img, image.Point{}, draw.Src)
		y += h
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: 95}); err != nil {
		return nil, fmt.Errorf("encode assembled image: %w", err)
	}
	return buf.Bytes(), nil
}

// PagesToPDF encodes a slice of assembled JPEG page bytes into a multi-page PDF.
func PagesToPDF(pages [][]byte) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetAutoPageBreak(false, 0)

	for i, pageJPEG := range pages {
		img, err := jpeg.Decode(bytes.NewReader(pageJPEG))
		if err != nil {
			return nil, fmt.Errorf("decode page %d for PDF: %w", i, err)
		}
		bounds := img.Bounds()
		imgW := float64(bounds.Dx())
		imgH := float64(bounds.Dy())

		pdf.AddPage()

		// A4 in mm: 210 × 297 — fit image to page width, preserving aspect ratio
		pageW, pageH := pdf.GetPageSize()
		scale := pageW / imgW
		displayH := imgH * scale
		if displayH > pageH {
			scale = pageH / imgH
			displayH = pageH
		}
		displayW := imgW * scale

		name := fmt.Sprintf("page%d", i)
		pdf.RegisterImageOptionsReader(name, fpdf.ImageOptions{ImageType: "JPEG"}, bytes.NewReader(pageJPEG))
		pdf.ImageOptions(name, 0, 0, displayW, displayH, false, fpdf.ImageOptions{ImageType: "JPEG"}, 0, "")
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("PDF output: %w", err)
	}
	return buf.Bytes(), nil
}

// PageToJPEG returns the first page as raw JPEG bytes (identity for single-page scans).
func PageToJPEG(page []byte) []byte {
	return page
}

// findSOI returns the byte offsets of every JPEG SOI marker (0xff 0xd8) in data.
func findSOI(data []byte) []int {
	var offsets []int
	start := 0
	for {
		idx := bytes.Index(data[start:], []byte{0xff, 0xd8})
		if idx < 0 {
			break
		}
		offsets = append(offsets, start+idx)
		start += idx + 2
	}
	return offsets
}

// splitStrips slices raw into individual strips based on SOI offsets.
func splitStrips(raw []byte, offsets []int) [][]byte {
	strips := make([][]byte, len(offsets))
	for i, off := range offsets {
		end := len(raw)
		if i+1 < len(offsets) {
			end = offsets[i+1]
		}
		strips[i] = raw[off:end]
	}
	return strips
}

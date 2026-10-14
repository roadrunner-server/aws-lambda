package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"testing"
)

func TestContentTypeClassification(t *testing.T) {
	tests := []struct {
		name   string
		method string
		ct     string
		expect int
	}{
		{"headSkipsBody", http.MethodHead, "application/json", contentNone},
		{"optionsSkipsBody", http.MethodOptions, "application/json", contentNone},
		{"urlencoded", http.MethodPost, "application/x-www-form-urlencoded", contentURLEncoded},
		{"multipart", http.MethodPost, "multipart/form-data; boundary=demo", contentMultipart},
		{"streamDefault", http.MethodPost, "application/json", contentStream},
	}

	for _, tt := range tests {
		if got := contentType(tt.method, tt.ct); got != tt.expect {
			t.Fatalf("%s: got %d want %d", tt.name, got, tt.expect)
		}
	}
}

func TestParseURLEncodedBuildsTree(t *testing.T) {
	body := []byte("full_name=Ada+Lovelace&nested[one]=1&tags[]=a&tags[]=b")

	encoded, err := parseURLEncoded(body, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if err != nil {
		t.Fatalf("parseURLEncoded error: %v", err)
	}

	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if data["full_name"] != "Ada Lovelace" {
		t.Fatalf("full_name mismatch: %v", data["full_name"])
	}

	nested, ok := data["nested"].(map[string]any)
	if !ok || nested["one"] != "1" {
		t.Fatalf("nested not parsed: %v", data["nested"])
	}

	tags, ok := data["tags"].([]any)
	if !ok || len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Fatalf("tags mismatch: %v", data["tags"])
	}
}

func TestParseMultipartReturnsFieldsAndUploads(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	if err := w.WriteField("title", "demo"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := w.WriteField("meta[author]", "mike"); err != nil {
		t.Fatalf("write field: %v", err)
	}

	fileWriter, err := w.CreateFormFile("file", "demo.png")
	if err != nil {
		t.Fatalf("create file field: %v", err)
	}
	content := []byte{0x89, 'P', 'N', 'G'}
	if _, err := fileWriter.Write(content); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	headers := map[string]string{
		"Content-Type": w.FormDataContentType(),
	}

	encoded, uploads, err := parseMultipart(buf.Bytes(), headers)
	if err != nil {
		t.Fatalf("parseMultipart error: %v", err)
	}
	if uploads == nil {
		t.Fatalf("uploads should not be nil")
	}
	if len(uploads.list) != 1 {
		t.Fatalf("expected 1 upload, got %d", len(uploads.list))
	}

	up := uploads.list[0]
	if up.Name != "demo.png" {
		t.Fatalf("upload name mismatch: %s", up.Name)
	}
	if up.TempFilename == "" || !exists(up.TempFilename) {
		t.Fatalf("temp file not created: %s", up.TempFilename)
	}
	if up.Size != int64(len(content)) {
		t.Fatalf("size mismatch: %d", up.Size)
	}

	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}

	if data["title"] != "demo" {
		t.Fatalf("title mismatch: %v", data["title"])
	}
	meta, ok := data["meta"].(map[string]any)
	if !ok || meta["author"] != "mike" {
		t.Fatalf("meta mismatch: %v", data["meta"])
	}

	uploads.Clear()
	if up.TempFilename != "" && exists(up.TempFilename) {
		t.Fatalf("expected temp file to be removed")
	}
}

func TestPackDataTreeEmpty(t *testing.T) {
	out, err := packDataTree(dataTree{})
	if err != nil {
		t.Fatalf("packDataTree error: %v", err)
	}
	if string(out) != "{}" {
		t.Fatalf("expected empty object, got %s", string(out))
	}
}

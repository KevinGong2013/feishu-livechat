package localblob

import (
	"context"
	"io"
	"testing"
)

func TestPutGetAndTraversal(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Put(ctx, "conv/msg-0.png", "image/png", []byte("data")); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Get(ctx, "conv/msg-0.png")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "data" {
		t.Fatalf("got %q", b)
	}
	for _, key := range []string{"", "../escape", "/etc/passwd", "a/../../escape", ".."} {
		if err := s.Put(ctx, key, "", []byte("x")); err == nil {
			t.Errorf("Put(%q) must fail", key)
		}
		if _, err := s.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) must fail", key)
		}
	}
}

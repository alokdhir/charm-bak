package ui

import (
	"slices"
	"testing"
)

// A form saves only from its buttons: Enter on a plain field moves on,
// Enter on Folders opens the picker, ↓ ends on Save, and Cancel closes
// without sending anything.
func TestFormButtons(t *testing.T) {
	a := newTestApp(t, 160, 40)
	before := a.sources[0]
	press(a, "e")
	f, ok := a.modal.(*sourceForm)
	if !ok {
		t.Fatalf("e: modal %T", a.modal)
	}
	typeText(a, "x")
	press(a, "enter")
	if a.modal != f || f.field != ffFolders {
		t.Fatalf("enter on Name: modal %T field %d, want Folders", a.modal, f.field)
	}
	press(a, "enter")
	if _, ok := a.modal.(*folderPicker); !ok {
		t.Fatalf("enter on Folders: modal %T, want the picker", a.modal)
	}
	press(a, "esc")
	if a.modal != f {
		t.Fatalf("esc in the picker: modal %T", a.modal)
	}
	for range 20 {
		press(a, "down")
	}
	if f.field != ffButtons || f.buttons.pick != 0 {
		t.Fatalf("↓ to the end: field %d pick %d, want Save", f.field, f.buttons.pick)
	}
	press(a, "right", "enter") // Cancel
	if a.modal != nil {
		t.Fatalf("Cancel: modal %T", a.modal)
	}
	if after := selSrc(t, a); after.Name != before.Name || !slices.Equal(after.Paths, before.Paths) {
		t.Errorf("Cancel saved: %q %q", after.Name, after.Paths)
	}
}

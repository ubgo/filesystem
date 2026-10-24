package raster

import (
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

// TestRendererRefusesBrokenXML covers the default renderer's own error:
// the sanitiser never hands it broken XML, so it is called directly.
func TestRendererRefusesBrokenXML(t *testing.T) {
	if _, err := renderOKSVG(strings.NewReader("<svg"), 1, 1); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("broken XML = %v", err)
	}
}

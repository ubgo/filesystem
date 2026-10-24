package svg

import "strings"

// setOf builds a lookup set.
func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// lowerSetOf builds a lookup set of lower-cased names, for matching
// attribute names without case as the attribute allowlist does.
func lowerSetOf(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[strings.ToLower(n)] = true
	}
	return m
}

// allowedElements are the SVG elements kept: shapes, text, structure,
// paint servers, clipping and masking, filters and animation. Everything
// else is dropped with its content: script, foreignObject (which embeds
// HTML), iframe, audio, video, canvas, metadata, and every element of
// another namespace. Names are as SVG spells them, case included.
var allowedElements = setOf(
	"svg", "g", "defs", "symbol", "use", "title", "desc", "switch", "view", "a",
	"path", "rect", "circle", "ellipse", "line", "polyline", "polygon",
	"text", "tspan", "textPath",
	"linearGradient", "radialGradient", "stop", "pattern", "clipPath", "mask", "marker", "image", "style",
	"filter", "feBlend", "feColorMatrix", "feComponentTransfer", "feComposite", "feConvolveMatrix",
	"feDiffuseLighting", "feDisplacementMap", "feDistantLight", "feDropShadow", "feFlood",
	"feFuncA", "feFuncB", "feFuncG", "feFuncR", "feGaussianBlur", "feImage", "feMerge", "feMergeNode",
	"feMorphology", "feOffset", "fePointLight", "feSpecularLighting", "feSpotLight", "feTile", "feTurbulence",
	"animate", "animateMotion", "animateTransform", "set", "mpath",
)

// unwrapped elements lose their tag but keep their children: a link's
// text and shapes stay visible, the link does not.
var unwrapped = setOf("a")

// animations are the elements whose attributeName is checked: animating
// href or an on* attribute would bring back what sanitising removed.
var animations = setOf("animate", "animateMotion", "animateTransform", "set")

// allowedAttributes are the attributes kept, lower-cased (SVG attribute
// names are matched without case here, so a mixed-case spelling cannot
// slip past). No on* event handler is in it.
var allowedAttributes = setOf(
	// Core, styling and accessibility.
	"id", "class", "style", "lang", "tabindex", "role", "focusable",
	"aria-label", "aria-labelledby", "aria-describedby", "aria-hidden",
	"xml:space", "xml:lang", "systemlanguage", "requiredfeatures", "requiredextensions",
	// Links inside the document (values are checked: #fragment only).
	"href", "xlink:href", "xlink:title",
	// Geometry and viewport.
	"x", "y", "x1", "y1", "x2", "y2", "cx", "cy", "r", "rx", "ry", "fx", "fy", "fr",
	"width", "height", "d", "points", "pathlength", "viewbox", "preserveaspectratio", "version",
	"transform", "transform-origin", "dx", "dy", "rotate", "textlength", "lengthadjust",
	"refx", "refy", "orient", "markerwidth", "markerheight", "markerunits", "zoomandpan",
	// Presentation.
	"fill", "fill-opacity", "fill-rule", "stroke", "stroke-width", "stroke-opacity",
	"stroke-dasharray", "stroke-dashoffset", "stroke-linecap", "stroke-linejoin", "stroke-miterlimit",
	"opacity", "color", "display", "visibility", "overflow", "clip", "clip-path", "clip-rule",
	"mask", "mask-type", "filter", "marker-start", "marker-mid", "marker-end", "paint-order", "vector-effect",
	"shape-rendering", "image-rendering", "text-rendering", "color-rendering", "color-interpolation",
	"color-interpolation-filters", "pointer-events", "mix-blend-mode", "isolation",
	"font-family", "font-size", "font-size-adjust", "font-stretch", "font-style", "font-variant", "font-weight",
	"letter-spacing", "word-spacing", "text-anchor", "text-decoration", "dominant-baseline",
	"alignment-baseline", "baseline-shift", "writing-mode", "direction", "unicode-bidi",
	"startoffset", "method", "spacing", "side",
	// Paint servers, clipping and masking.
	"offset", "stop-color", "stop-opacity", "gradientunits", "gradienttransform", "spreadmethod",
	"patternunits", "patterncontentunits", "patterntransform", "clippathunits", "maskunits", "maskcontentunits",
	// Filters.
	"filterunits", "primitiveunits", "in", "in2", "result", "mode", "type", "values", "stddeviation",
	"edgemode", "operator", "k1", "k2", "k3", "k4", "order", "kernelmatrix", "divisor", "bias",
	"targetx", "targety", "preservealpha", "kernelunitlength", "surfacescale", "diffuseconstant",
	"specularconstant", "specularexponent", "lighting-color", "azimuth", "elevation", "pointsatx",
	"pointsaty", "pointsatz", "limitingconeangle", "scale", "xchannelselector", "ychannelselector",
	"flood-color", "flood-opacity", "radius", "basefrequency", "numoctaves", "seed", "stitchtiles",
	"tablevalues", "slope", "intercept", "amplitude", "exponent", "z",
	// Animation (attributeName is checked on the element).
	"attributename", "attributetype", "begin", "dur", "end", "min", "max", "restart", "repeatcount",
	"repeatdur", "calcmode", "keytimes", "keysplines", "keypoints", "from", "to", "by",
	"additive", "accumulate", "path",
)

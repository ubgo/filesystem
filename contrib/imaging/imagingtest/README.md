# github.com/ubgo/filesystem/contrib/imaging/imagingtest

Golden-image tests by perceptual difference: a test's output image is compared with a stored PNG by how different it looks, so a level of rounding from an encoder passes and a wrong crop fails. The guide is [Image processing: golden image tests](../../../docs/plugins/image-processing.md#colour-profiles-svg-and-golden-tests).

```go
imagingtest.Golden(t, got, "testdata/card.golden.png")
```

| Symbol | What it is |
|---|---|
| `Golden(t, got, path, ...Options)` | compares `got` with the PNG at `path` and fails `t` past the limits, saving `got` in a temporary directory it names; with `-update` (the test package's own flag) or `Options.Update` writes `path` instead |
| `Compare(a, b, threshold) (Diff, error)` | the mean and largest per-pixel distance (colour weighted by perceived brightness, over mid grey for transparency, 0 to 255) and the fraction of pixels past `threshold` |
| `Options{MaxMean, PixelThreshold, MaxDifferent, Update}` | limits; zero means `DefaultMaxMean` (1), `DefaultPixelThreshold` (16), `DefaultMaxDifferent` (0.1%) |

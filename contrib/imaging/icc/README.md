# github.com/ubgo/filesystem/contrib/imaging/icc

Converts images from the colour space of an embedded ICC profile to sRGB, for `imaging.WithColourManagement`, in pure Go with only the standard library. The guide is [Image processing: colour profiles](../../../docs/plugins/image-processing.md#colour-profiles-svg-and-golden-tests).

```go
p := imaging.NewPipeline(imaging.WithColourManagement(icc.ToSRGB))
```

## Go API

| Symbol | What it is |
|---|---|
| `ToSRGB(profile, img) (image.Image, bool, error)` | an `imaging.ColourConverter`: converts when `profile` is a supported RGB matrix/TRC profile, otherwise reports `false` so imaging keeps the profile; never fails |
| `Parse(data) (*Profile, error)` | reads a profile: `ErrUnsupported` for anything but RGB with an XYZ connection space, colorants and tone curves (`curv` gamma, table or identity, `para` types 0 to 4); an error for a malformed one |
| `(*Profile).ToSRGB(img) *image.NRGBA` | the conversion: the profile's tone curves and colorants to XYZ (D50), then sRGB's inverse; alpha kept, colours outside sRGB clipped |

## Limits

- Lookup-table profiles (`A2B0`), and CMYK and grey profiles, are unsupported: imaging keeps them in the output instead. Pass a converter built on lcms2 (its own module) for those.
- A profile whose curves are not finite for every 8-bit input, or whose curves times colorants could overflow, is malformed and refused at `Parse`, so a crafted profile cannot produce NaN in the conversion.
- Pixels are read at 8 bits per channel; 16-bit input is rounded.
- The tests check Display P3 against the published P3 to sRGB matrix (within three levels), sRGB as an identity, and every curve type.

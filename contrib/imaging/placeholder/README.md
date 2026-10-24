# github.com/ubgo/filesystem/contrib/imaging/placeholder

The image shown for a user, team or item with no picture of its own: up to two initials of a display name on a coloured square, or an identicon (a mirrored 5×5 pattern) for a seed with no usable name. It stores nothing and needs no records Store. The guide is [Avatars: when there is no avatar](../../../docs/recipes/avatars.md#when-there-is-no-avatar).

## Install

```sh
go get github.com/ubgo/filesystem/contrib/imaging
```

The package is in the `contrib/imaging` module. The text is drawn with the Go Bold font embedded in `golang.org/x/image`, which the module already requires; nothing else is added.

## Behaviour

- **Deterministic.** The same seed, name, size and style always give the same pixels and the same PNG bytes. The seed (a user ID) picks one of 13 background colours and the identicon pattern from its SHA-256; the name gives the initials.
- **Initials.** `Initials(name)` takes the first letter or digit of the first and of the last word, upper-cased: `Ada Lovelace` → `AL`, `Ада Лавлейс` → `АЛ`, `@ada` → `A`. Punctuation and emoji are skipped; invalid UTF-8 counts as no name. Two names with the same initials share one image and one ETag.
- **Never missing glyphs.** Go Bold covers Latin, Greek and Cyrillic. A name whose initials it cannot draw (CJK, emoji only, no letters) gets the seed's identicon instead.
- **Bounded.** Sizes are `MinSize` to `MaxSize` (16 to 512 px, default `DefaultSize` 128); seed and name are at most 256 bytes each. Anything else is `CodeInvalidArgument` (400 from the handler).
- **Versioned.** `Version` is part of every ETag and changes whenever the drawing changes; put it in fallback URLs so caches that never revalidate pick up a new look.

## Go API

| Symbol | What it does |
|---|---|
| `PNG(Request) ([]byte, error)` | Draws the placeholder |
| `ETag(Request) (string, error)` | Its quoted entity tag, computed without drawing |
| `Initials(name string) string` | The initials `StyleInitials` draws, or `""` |
| `Request{Seed, Name, Size, Style}` | `Seed` required; `Size` 0 means `DefaultSize`; `Style` empty means `StyleInitials` |
| `StyleInitials`, `StyleIdenticon` | Initials (falling back to the identicon), or always the identicon |
| `Handler(HandlerOptions) http.Handler` | `GET`/`HEAD /{seed}` or `/{seed}.png` with optional `name`, `size`, `style` query parameters (`ParamName`, `ParamSize`, `ParamStyle`); mount with the prefix stripped |
| `HandlerOptions.Name` | Optional lookup from seed to display name; the `name` parameter is then ignored, and a lookup error is answered with its code's status (`fshttp.StatusOf`) |
| `HandlerOptions.Style` | The style when the request has none |
| `HandlerOptions.CacheControl` | Overrides `Cache-Control`; empty means `CacheControlImmutable` (`public, max-age=31536000, immutable`), or `CacheControlLookup` (`public, max-age=3600`) with `Name`, since a rename changes the image under the same URL |
| `MinSize`, `MaxSize`, `DefaultSize`, `MaxSeedBytes`, `MaxNameBytes`, `Version` | The bounds and the drawing version |

## Responses

| Situation | Status | Headers |
|---|---|---|
| drawn | 200, PNG body (none for `HEAD`) | `Content-Type: image/png`, `ETag`, `Cache-Control`, `Content-Length`, `X-Content-Type-Options: nosniff` |
| `If-None-Match` lists the ETag (weak or strong) or is `*` | 304, nothing drawn | `ETag`, `Cache-Control` |
| path not one segment, size not a number or out of bounds, unknown style, seed or name too long | 400 | |
| `HandlerOptions.Name` failed | the status of its code | |
| any other method | 405 | `Allow: GET, HEAD` |

## Limits

- Initials of scripts Go Bold does not cover fall back to the identicon; there is no font fallback chain.
- Output is PNG only, square only.

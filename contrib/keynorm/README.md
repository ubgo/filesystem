# contrib/keynorm

`github.com/ubgo/filesystem/contrib/keynorm` holds Unicode key normalisers for `filesystem.WithKeyNormalizer`, in a module of its own because it needs `golang.org/x/text`, which the core does not import.

macOS file pickers send `café` decomposed (NFD), most other systems composed (NFC); without normalisation they are two keys, and a file uploaded from one machine is not found from another.

```go
disk, err := filesystem.NewDisk("media", drv, filesystem.WithKeyNormalizer(keynorm.NFC))
```

| Name | What it is |
|---|---|
| `NFC(key)` | the key in Unicode Normalization Form C; changes only the Unicode form (no case folding, no compatibility mapping, so it can never turn a key into another path); invalid UTF-8 is `CodeInvalidKey` |

The guide is [Keys](../../docs/concepts/keys.md#unicode-spellings).

# github.com/ubgo/filesystem/contrib/imaging/focal

A records plugin that stores a focal point per file (the face in a portrait, the product in a scene) as two fractions in its metadata, so variants declared with `imaging.GravityFocal` crop around it. The guide is [Image variants: focal points](../../../docs/plugins/variants.md#focal-points-and-format-negotiation).

## Wire it

```go
fp := focal.New()
vp, err := variants.New(transforms, imaging.Options{}, variants.WithFocal(fp))
store, err := records.New(db, dialect, disks, records.WithPlugins(fp, vp))
_, err = fp.Set(ctx, store, scope, fileID, &imaging.Focal{X: 0.7, Y: 0.3})
```

The plugin has no table and no migration.

## Go API

| Symbol | What it does |
|---|---|
| `New() *Plugin` | the plugin, for `records.WithPlugins` |
| `(*Plugin).Set(ctx, s, scope, fileID, pt *imaging.Focal) (records.File, error)` | stores `pt` (fractions within 0..1), or clears it when nil, locking the file's row and emitting `EventChanged` with the file in the same transaction; the same point again is a no-op. Out of range is `CodeInvalidArgument`, an unknown or trashed file `CodeNotFound` |
| `Of(f records.File) (imaging.Focal, bool)` | the stored point; false when none or not within 0..1 |
| `(*Plugin).Focal`, `(*Plugin).FocalChanged` | implement `variants.FocalSource` |
| `MetaX`, `MetaY` | the metadata keys, `focal_x` and `focal_y`; set them in `records.NewFile.Metadata` to give a point at upload |
| `EventChanged`, `DetailX`, `DetailY` | `focal.changed` and its `Detail` keys (the new point, empty when cleared) |

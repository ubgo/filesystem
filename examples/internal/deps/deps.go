// Package deps imports every module of the repository, so the examples
// module requires them all and any example (or guide program) can import
// any package without editing go.mod.
package deps

import (
	// In-process fakes, so example tests run without cloud accounts.
	_ "github.com/fsouza/fake-gcs-server/fakestorage"
	_ "github.com/johannesboyne/gofakes3"
	_ "github.com/johannesboyne/gofakes3/backend/s3mem"
	_ "github.com/pkg/sftp"

	_ "github.com/ubgo/filesystem/adapters/azure"
	_ "github.com/ubgo/filesystem/adapters/gcs"
	_ "github.com/ubgo/filesystem/adapters/s3"
	_ "github.com/ubgo/filesystem/adapters/sftp"
	_ "github.com/ubgo/filesystem/contrib/imaging"
	_ "github.com/ubgo/filesystem/contrib/imaging/dimensions"
	_ "github.com/ubgo/filesystem/contrib/imaging/variants"
	_ "github.com/ubgo/filesystem/contrib/otel"
	_ "github.com/ubgo/filesystem/contrib/tus"
	_ "github.com/ubgo/filesystem/records"

	// Modules the guide examples use, kept required here so that go mod
	// tidy does not drop one while no example imports it yet.
	_ "github.com/ubgo/filesystem/adapters/cloudinary"
	_ "github.com/ubgo/filesystem/adapters/dropbox"
	_ "github.com/ubgo/filesystem/adapters/gdrive"
	_ "github.com/ubgo/filesystem/adapters/onedrive"
	_ "github.com/ubgo/filesystem/adapters/vercelblob"
	_ "github.com/ubgo/filesystem/contrib/ent"
	_ "github.com/ubgo/filesystem/contrib/gorm"
	_ "github.com/ubgo/filesystem/contrib/keynorm"
	_ "github.com/ubgo/filesystem/contrib/notify"
	_ "github.com/ubgo/filesystem/contrib/notify/eventgrid"
	_ "github.com/ubgo/filesystem/contrib/notify/pubsub"
	_ "github.com/ubgo/filesystem/contrib/notify/sqs"
	_ "github.com/ubgo/filesystem/contrib/sqlc"

	// Third-party packages the ORM example needs: the GORM dialector over
	// the Store's *sql.DB, and the schema package every generated ent
	// client imports.
	_ "entgo.io/ent/dialect/sql/schema"
	_ "github.com/ncruces/go-sqlite3/gormlite"
)

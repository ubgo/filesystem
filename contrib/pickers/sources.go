package pickers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/ubgo/filesystem"
)

// Default API endpoints of the built-in sources.
const (
	DefaultDriveURL      = "https://www.googleapis.com/drive/v3"
	DefaultDropboxAPIURL = "https://api.dropboxapi.com/2"
	DefaultDropboxURL    = "https://content.dropboxapi.com/2"
	DefaultOneDriveURL   = "https://graph.microsoft.com/v1.0"
)

// ID shapes the built-in sources accept, so a pick can never steer a
// request to another path of the vendor's API.
var (
	driveID    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	dropboxID  = regexp.MustCompile(`^id:[A-Za-z0-9_-]+$`)
	onedriveID = regexp.MustCompile(`^[A-Za-z0-9!_-]+$`)
)

func checkID(re *regexp.Regexp, id, what string) error {
	if !re.MatchString(id) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "pickers: %q is not a %s ID", id, what)
	}
	return nil
}

func getJSON(ctx context.Context, c *http.Client, target, what string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	resp, err := send(c, req, what)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBody)).Decode(out); err != nil {
		return filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	return nil
}

func get(ctx context.Context, c *http.Client, target, what string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	resp, err := send(c, req, what)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// GoogleDrive imports files picked with the Google Picker (Pick.ID is the
// Drive file ID). Google Docs, Sheets and other native files have no bytes
// and are CodeUnsupported, unless Export names a format to convert them
// to.
type GoogleDrive struct {
	// APIURL overrides DefaultDriveURL, for a test server.
	APIURL string
	// Export maps a Google-native type ("application/vnd.google-apps.
	// document") to the format it is imported as, through Drive's
	// files.export: a Doc as PDF or DOCX, a Sheet as XLSX, as Uppy
	// Companion does. A native type it does not name stays
	// CodeUnsupported; nil (the default) refuses every native file. The
	// exported bytes go through the upload policy like any other: the
	// type is detected from them, never taken from this map. Drive
	// exports at most 10 MB per file.
	Export map[string]DriveExport
}

// DriveExport is the format a Google-native file is exported as.
type DriveExport struct {
	// MimeType is the export format Drive is asked for
	// ("application/pdf").
	MimeType string
	// Extension is added to the file name (".pdf"), since a native
	// file's name has none.
	Extension string
}

// googleNative starts the type of every Google-native file.
const googleNative = "application/vnd.google-apps."

// driveMeta is the part of files.get the source reads.
type driveMeta struct {
	Name     string `json:"name"`
	Size     string `json:"size"`
	MimeType string `json:"mimeType"`
}

// meta reads p's metadata with files.get.
func (s GoogleDrive) meta(ctx context.Context, c *http.Client, p Pick) (driveMeta, error) {
	var f driveMeta
	target := endpointOr(s.APIURL, DefaultDriveURL) + "/files/" + p.ID + "?fields=name,size,mimeType&supportsAllDrives=true"
	err := getJSON(ctx, c, target, "drive files.get", &f)
	return f, err
}

// Origins implements Source.
func (s GoogleDrive) Origins() []string { return []string{endpointOr(s.APIURL, DefaultDriveURL)} }

// Stat implements Source with files.get.
func (s GoogleDrive) Stat(ctx context.Context, c *http.Client, p Pick) (File, error) {
	if err := checkID(driveID, p.ID, "Drive file"); err != nil {
		return File{}, err
	}
	f, err := s.meta(ctx, c, p)
	if err != nil {
		return File{}, err
	}
	if strings.HasPrefix(f.MimeType, googleNative) {
		ex, ok := s.Export[f.MimeType]
		if !ok {
			return File{}, filesystem.Errorf(filesystem.CodeUnsupported, "pickers: %s is a Google-native document with no bytes to import", p.ID)
		}
		// An export's size is known only once it is made.
		return File{Name: f.Name + ex.Extension, Size: -1}, nil
	}
	size, err := strconv.ParseInt(f.Size, 10, 64)
	if err != nil {
		size = -1
	}
	return File{Name: f.Name, Size: size}, nil
}

// Open implements Source with alt=media, or files.export for a native
// file Export names. With Export set it reads the metadata again to tell
// which (a Source keeps no state between Stat and Open); without it, it
// makes the one request it always made.
func (s GoogleDrive) Open(ctx context.Context, c *http.Client, p Pick) (io.ReadCloser, error) {
	if err := checkID(driveID, p.ID, "Drive file"); err != nil {
		return nil, err
	}
	base := endpointOr(s.APIURL, DefaultDriveURL) + "/files/" + p.ID
	if len(s.Export) > 0 {
		f, err := s.meta(ctx, c, p)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(f.MimeType, googleNative) {
			ex, ok := s.Export[f.MimeType]
			if !ok {
				return nil, filesystem.Errorf(filesystem.CodeUnsupported, "pickers: %s is a Google-native document with no bytes to import", p.ID)
			}
			return get(ctx, c, base+"/export?mimeType="+url.QueryEscape(ex.MimeType)+"&supportsAllDrives=true", "drive export")
		}
	}
	return get(ctx, c, base+"?alt=media&supportsAllDrives=true", "drive media")
}

// Dropbox imports files by Dropbox file ID (Pick.ID "id:…", which the
// Chooser and the API both report), with the user's token. Dropbox answers
// a missing file with 409, which is CodeNotFound here.
type Dropbox struct {
	// APIURL and ContentURL override DefaultDropboxAPIURL and
	// DefaultDropboxURL, for a test server.
	APIURL, ContentURL string
}

// Origins implements Source.
func (s Dropbox) Origins() []string {
	return []string{endpointOr(s.APIURL, DefaultDropboxAPIURL), endpointOr(s.ContentURL, DefaultDropboxURL)}
}

// Stat implements Source with files/get_metadata; a folder is
// CodeUnsupported.
func (s Dropbox) Stat(ctx context.Context, c *http.Client, p Pick) (File, error) {
	arg, err := dropboxArg(p)
	if err != nil {
		return File{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointOr(s.APIURL, DefaultDropboxAPIURL)+"/files/get_metadata", strings.NewReader(arg))
	if err != nil {
		return File{}, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := send(c, req, "dropbox get_metadata", http.StatusConflict)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var m struct {
		Tag  string `json:".tag"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBody)).Decode(&m); err != nil {
		return File{}, filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	if m.Tag != "file" {
		return File{}, filesystem.Errorf(filesystem.CodeUnsupported, "pickers: %s is not a file", p.ID)
	}
	return File{Name: m.Name, Size: m.Size}, nil
}

// Open implements Source with files/download.
func (s Dropbox) Open(ctx context.Context, c *http.Client, p Pick) (io.ReadCloser, error) {
	arg, err := dropboxArg(p)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointOr(s.ContentURL, DefaultDropboxURL)+"/files/download", nil)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	req.Header.Set("Dropbox-API-Arg", arg) // ASCII: the ID shape is checked
	resp, err := send(c, req, "dropbox download", http.StatusConflict)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func dropboxArg(p Pick) (string, error) {
	if err := checkID(dropboxID, p.ID, "Dropbox file"); err != nil {
		return "", err
	}
	arg, err := json.Marshal(map[string]string{"path": p.ID})
	if err != nil {
		return "", filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	return string(arg), nil
}

// OneDrive imports files picked with the OneDrive and SharePoint file
// picker (Pick.ID the item ID, Pick.DriveID its drive). Graph answers the
// content request with a redirect to a pre-authenticated host, which never
// receives the token.
type OneDrive struct {
	// BaseURL overrides DefaultOneDriveURL, for a national cloud or a
	// test server.
	BaseURL string
}

// Origins implements Source.
func (s OneDrive) Origins() []string { return []string{endpointOr(s.BaseURL, DefaultOneDriveURL)} }

func (s OneDrive) item(p Pick) (string, error) {
	if err := checkID(onedriveID, p.ID, "OneDrive item"); err != nil {
		return "", err
	}
	drive := "/me/drive"
	if p.DriveID != "" {
		if err := checkID(onedriveID, p.DriveID, "OneDrive drive"); err != nil {
			return "", err
		}
		drive = "/drives/" + url.PathEscape(p.DriveID)
	}
	return endpointOr(s.BaseURL, DefaultOneDriveURL) + drive + "/items/" + url.PathEscape(p.ID), nil
}

// Stat implements Source with a driveItem request; a folder is
// CodeUnsupported.
func (s OneDrive) Stat(ctx context.Context, c *http.Client, p Pick) (File, error) {
	target, err := s.item(p)
	if err != nil {
		return File{}, err
	}
	var it struct {
		Name string    `json:"name"`
		Size int64     `json:"size"`
		File *struct{} `json:"file"`
	}
	if err := getJSON(ctx, c, target+"?$select=name,size,file", "graph item", &it); err != nil {
		return File{}, err
	}
	if it.File == nil {
		return File{}, filesystem.Errorf(filesystem.CodeUnsupported, "pickers: %s is not a file", p.ID)
	}
	return File{Name: it.Name, Size: it.Size}, nil
}

// Open implements Source with GET …/content.
func (s OneDrive) Open(ctx context.Context, c *http.Client, p Pick) (io.ReadCloser, error) {
	target, err := s.item(p)
	if err != nil {
		return nil, err
	}
	return get(ctx, c, target+"/content", "graph content")
}

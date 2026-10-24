package azure

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/ubgo/filesystem"
)

// Headers a signed PUT must carry: Azure creates a block blob only when
// told the blob type.
const (
	headerBlobType = "x-ms-blob-type"
	blobTypeBlock  = "BlockBlob"
)

// Presign implements filesystem.Presigner with a service SAS signed by the
// shared key, or a user delegation SAS (Config.UserDelegation). GET permits
// reading (with optional response content type and disposition); PUT
// permits creating or overwriting the one blob. Azure signs no request
// headers, so a PUT cannot pin the Content-Type: Header carries it, the
// client must send it, and Stat reports what was stored, so
// upload.Confirm can refuse an upload whose stored type differs.
// PresignOptions.AllowedIP becomes the SAS sip range, so Azure refuses a
// request from any other address (PresignIP); Azure takes IPv4 ranges
// only, so an IPv6 prefix is CodeUnsupported.
func (d *Driver) Presign(ctx context.Context, key string, opts filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	if err := checkKey(key); err != nil {
		return filesystem.SignedRequest{}, err
	}
	blobURL := d.container.NewBlobClient(key).URL()
	expires := d.now().Add(opts.Expires).UTC()
	v := sas.BlobSignatureValues{
		Protocol:      sas.ProtocolHTTPS,
		ExpiryTime:    expires,
		ContainerName: d.cfg.Container,
		BlobName:      key,
	}
	if strings.HasPrefix(blobURL, "http://") {
		v.Protocol = sas.ProtocolHTTPSandHTTP // Azurite and other local endpoints
	}
	if opts.AllowedIP != "" {
		r, err := sasIPRange(opts.AllowedIP)
		if err != nil {
			return filesystem.SignedRequest{}, err
		}
		v.IPRange = r
	}
	header := http.Header{}
	if opts.Method == filesystem.MethodPut {
		ro := requestOptions(ctx, key)
		if err := onlyScopeAndTags(ro, "a presigned PUT", false); err != nil {
			return filesystem.SignedRequest{}, err
		}
		ws, err := d.settings(key, ro)
		if err != nil {
			return filesystem.SignedRequest{}, err
		}
		v.Permissions = (&sas.BlobPermissions{Create: true, Write: true}).String()
		v.EncryptionScope = ws.scope
		header.Set(headerBlobType, blobTypeBlock)
		if opts.ContentType != "" {
			header.Set("Content-Type", opts.ContentType)
		}
	} else {
		v.Permissions = (&sas.BlobPermissions{Read: true}).String()
		v.ContentType = opts.ContentType
		v.ContentDisposition = opts.ContentDisposition
		header = nil
	}
	var q sas.QueryParameters
	var err error
	if d.cfg.UserDelegation {
		var cred *sas.UserDelegationCredential
		var keyExp time.Time
		if cred, keyExp, err = d.delegationKey(ctx, expires); err == nil {
			// A user delegation SAS cannot outlive its key.
			if keyExp.Before(expires) {
				expires = keyExp
				v.ExpiryTime = expires
			}
			q, err = v.SignWithUserDelegation(cred)
		}
	} else {
		q, err = v.SignWithSharedKey(d.cfg.SharedKey)
	}
	if err != nil {
		return filesystem.SignedRequest{}, mapErr(err)
	}
	return filesystem.SignedRequest{Method: opts.Method, URL: blobURL + "?" + q.Encode(), Header: header, ExpiresAt: expires}, nil
}

// sasIPRange turns a canonical IPv4 CIDR prefix into the first and last
// address of a SAS sip range. A /32 is a single address, which Azure
// writes without an end.
func sasIPRange(cidr string) (sas.IPRange, error) {
	p, err := filesystem.ParseAllowedIP(cidr)
	if err != nil {
		return sas.IPRange{}, err
	}
	if !p.Addr().Is4() {
		return sas.IPRange{}, filesystem.Errorf(filesystem.CodeUnsupported, "azure: a SAS restricts IPv4 ranges only, not %s", cidr)
	}
	first := p.Addr().As4()
	host := uint32(1)<<(ipv4Bits-p.Bits()) - 1
	last := binary.BigEndian.Uint32(first[:]) | host
	r := sas.IPRange{Start: net.IP(first[:])}
	if host != 0 {
		r.End = binary.BigEndian.AppendUint32(nil, last)
	}
	return r, nil
}

// ipv4Bits is the length of an IPv4 address in bits.
const ipv4Bits = 32

// UserDelegationCredential returns the user delegation key the disk signs
// with, and when it expires, for signing a SAS outside Presign: pass it
// as DelegateOptions.UserDelegation. It is the driver's own key, fetched
// and cached exactly as for Presign (Config.UserDelegation), so it is
// bound to Config.AccountName when that is set: the one way to delegate
// a prefix from a client behind a custom domain or Front Door, which the
// SDK's GetUserDelegationCredential would bind to the wrong account. The
// key is valid at least until until, less an hour of reuse slack, and a
// SAS signed with it must not outlive the returned expiry. A disk without
// UserDelegation is CodeUnsupported; a missing role is CodePermission.
func (d *Driver) UserDelegationCredential(ctx context.Context, until time.Time) (*sas.UserDelegationCredential, time.Time, error) {
	if !d.cfg.UserDelegation {
		return nil, time.Time{}, filesystem.Errorf(filesystem.CodeUnsupported, "azure: UserDelegationCredential needs Config.UserDelegation")
	}
	cred, exp, err := d.delegationKey(ctx, until)
	if err != nil {
		return nil, time.Time{}, mapErr(err)
	}
	return cred, exp, nil
}

// delegationKey returns a user delegation key and its expiry. The cached
// key is reused while it lasts until until, less delegationKeyReuse;
// otherwise a new one is fetched, valid MaxUserDelegationKeyLifetime less
// delegationKeySkew (the most Azure grants, with room for clock skew). The
// caller clamps the URL to the key's expiry. The lock is held across the
// fetch, so concurrent callers wait for one request instead of each making
// their own.
func (d *Driver) delegationKey(ctx context.Context, until time.Time) (*sas.UserDelegationCredential, time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.delegation != nil && !d.delegExp.Before(until.Add(-delegationKeyReuse)) {
		return d.delegation, d.delegExp, nil
	}
	now := d.now().UTC()
	start, expiry := now.Add(-delegationKeySkew), now.Add(MaxUserDelegationKeyLifetime-delegationKeySkew)
	info := service.KeyInfo{Start: ptr(start.Format(sas.TimeFormat)), Expiry: ptr(expiry.Format(sas.TimeFormat))}
	cred, err := d.delegationClient.GetUserDelegationCredential(ctx, info, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	// sas.TimeFormat has whole seconds, so the key ends no later than this.
	d.delegation, d.delegExp = cred, expiry.Truncate(time.Second)
	return cred, d.delegExp, nil
}

// ptr returns a pointer to v, for the SDK's optional request fields.
func ptr[T any](v T) *T { return &v }

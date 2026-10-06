// Package auth implements invite-only passwordless authentication: passkeys
// (WebAuthn) and email magic links, with opaque session tokens stored
// hashed in Postgres.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/store"
)

// Errors returned to handlers.
var (
	ErrInvalidToken  = errors.New("auth: invalid or expired token")
	ErrNotInvited    = errors.New("auth: email not invited")
	ErrAlreadyExists = errors.New("auth: user already exists")
	ErrUnauthorized  = errors.New("auth: unauthorized")
)

// Durations.
const (
	SessionTTL      = 30 * 24 * time.Hour
	InviteTTL       = 7 * 24 * time.Hour
	MagicLinkTTL    = 15 * time.Minute
	WebAuthnCereTTL = 5 * time.Minute
	SessionCookie   = "ws_session"
)

// Mailer sends transactional email. Resend implements it; a log mailer is
// used in dev.
type Mailer interface {
	Send(ctx context.Context, to, subject, text, html string) error
}

// ErrPasskeysUnavailable is returned when WebAuthn could not be configured
// (for example the app is served on a bare IP, which cannot be a relying
// party ID). Magic links keep working.
var ErrPasskeysUnavailable = errors.New("auth: passkeys unavailable on this host; use a domain name over https")

// Service is the auth facade used by HTTP handlers.
type Service struct {
	db        *store.DB
	wa        *webauthn.WebAuthn // nil when passkeys are unavailable
	mailer    Mailer
	publicURL string
	secure    bool // Secure cookie flag
}

// PasskeysEnabled reports whether WebAuthn ceremonies can run.
func (s *Service) PasskeysEnabled() bool { return s.wa != nil }

// Config for the service.
type Config struct {
	RPID          string
	RPDisplayName string
	RPOrigins     []string
	PublicURL     string
	SecureCookies bool
}

// New constructs the service.
func New(db *store.DB, mailer Mailer, cfg Config) (*Service, error) {
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.RPID,
		RPDisplayName: cfg.RPDisplayName,
		RPOrigins:     cfg.RPOrigins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationPreferred,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: 2 * time.Minute, TimeoutUVD: 2 * time.Minute},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: 2 * time.Minute, TimeoutUVD: 2 * time.Minute},
		},
	})
	if err != nil {
		// Not fatal: serve the app with magic links only and say why.
		slog.Warn("passkeys disabled: WebAuthn configuration rejected (serve on a domain over https to enable them)", "rp_id", cfg.RPID, "err", err)
		wa = nil
	}
	return &Service{db: db, wa: wa, mailer: mailer, publicURL: strings.TrimRight(cfg.PublicURL, "/"), secure: cfg.SecureCookies}, nil
}

// ---- tokens ----

func newToken() (raw string, hash []byte) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(raw))
	return raw, h[:]
}

func hashToken(raw string) []byte {
	h := sha256.Sum256([]byte(raw))
	return h[:]
}

// ---- invites ----

// Invite creates an invite and emails the link. Returns the raw link.
func (s *Service) Invite(ctx context.Context, email string, invitedBy uuid.UUID, role string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if _, err := s.db.GetUserByEmail(ctx, email); err == nil {
		return "", ErrAlreadyExists
	}
	raw, hash := newToken()
	if role == "" {
		role = "member"
	}
	_, err := s.db.CreateInvite(ctx, store.CreateInviteParams{
		Email: email, TokenHash: hash, InvitedBy: uuid.NullUUID{UUID: invitedBy, Valid: invitedBy != uuid.Nil},
		Role: role, ExpiresAt: time.Now().Add(InviteTTL),
	})
	if err != nil {
		return "", err
	}
	link := s.publicURL + "/invite/" + raw
	if s.mailer != nil {
		_ = s.mailer.Send(ctx, email, "You're invited to ws",
			"You've been invited. Open this link to set up your account (expires in 7 days):\n\n"+link,
			fmt.Sprintf(`<p>You've been invited to <b>ws</b>.</p><p><a href="%s">Accept invite</a> (expires in 7 days)</p>`, link))
	}
	return link, nil
}

// BootstrapOwner creates the owner invite on first boot if no users exist.
// Returns the invite link, or "" if nothing was done.
func (s *Service) BootstrapOwner(ctx context.Context, ownerEmail string) (string, error) {
	if ownerEmail == "" {
		return "", nil
	}
	n, err := s.db.CountUsers(ctx)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return "", nil
	}
	// Reuse a pending invite if one exists so restarts don't spam.
	invites, err := s.db.ListInvites(ctx)
	if err == nil {
		for _, inv := range invites {
			if inv.Email == strings.ToLower(ownerEmail) && inv.UsedAt == nil && inv.ExpiresAt.After(time.Now()) {
				return "", nil // already pending; link was logged when created
			}
		}
	}
	return s.Invite(ctx, ownerEmail, uuid.Nil, "owner")
}

// AcceptInvite validates an invite token and creates the user. The caller
// then registers a passkey or sends a magic link. Returns the new user.
func (s *Service) AcceptInvite(ctx context.Context, rawToken, displayName string) (*store.User, error) {
	inv, err := s.db.GetInviteByTokenHash(ctx, hashToken(rawToken))
	if err != nil {
		return nil, ErrInvalidToken
	}
	if u, err := s.db.GetUserByEmail(ctx, inv.Email); err == nil {
		// Invite re-used after account creation: just mark it and return the user.
		_ = s.db.UseInvite(ctx, store.UseInviteParams{ID: inv.ID, UsedBy: uuid.NullUUID{UUID: u.ID, Valid: true}})
		return &u, nil
	}
	if displayName == "" {
		displayName = strings.Split(inv.Email, "@")[0]
	}
	u, err := s.db.CreateUser(ctx, store.CreateUserParams{Email: inv.Email, DisplayName: displayName, Role: inv.Role})
	if err != nil {
		return nil, err
	}
	if err := s.db.UseInvite(ctx, store.UseInviteParams{ID: inv.ID, UsedBy: uuid.NullUUID{UUID: u.ID, Valid: true}}); err != nil {
		return nil, err
	}
	return &u, nil
}

// PeekInvite returns the email for a valid invite token without consuming it.
func (s *Service) PeekInvite(ctx context.Context, rawToken string) (string, error) {
	inv, err := s.db.GetInviteByTokenHash(ctx, hashToken(rawToken))
	if err != nil {
		return "", ErrInvalidToken
	}
	return inv.Email, nil
}

// ---- magic links ----

// SendMagicLink emails a one-time login link. It does not reveal whether
// the email exists.
func (s *Service) SendMagicLink(ctx context.Context, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	u, err := s.db.GetUserByEmail(ctx, email)
	if err != nil {
		return nil // silent: don't leak membership
	}
	raw, hash := newToken()
	if _, err := s.db.CreateMagicLink(ctx, store.CreateMagicLinkParams{UserID: u.ID, TokenHash: hash, ExpiresAt: time.Now().Add(MagicLinkTTL)}); err != nil {
		return err
	}
	link := s.publicURL + "/login/magic/" + raw
	if s.mailer == nil {
		return nil
	}
	return s.mailer.Send(ctx, email, "Your ws sign-in link",
		"Click to sign in (expires in 15 minutes):\n\n"+link,
		fmt.Sprintf(`<p><a href="%s">Sign in to ws</a> (expires in 15 minutes)</p>`, link))
}

// ConsumeMagicLink validates the token and returns the user.
func (s *Service) ConsumeMagicLink(ctx context.Context, rawToken string) (*store.User, error) {
	ml, err := s.db.ConsumeMagicLink(ctx, hashToken(rawToken))
	if err != nil {
		return nil, ErrInvalidToken
	}
	u, err := s.db.GetUserByID(ctx, ml.UserID)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ---- sessions ----

// Principal is the authenticated identity attached to a request.
type Principal struct {
	UserID      uuid.UUID
	Email       string
	DisplayName string
	Role        string
	SessionID   uuid.UUID
	APIKeyID    uuid.NullUUID // set when authenticated by API key
	// DefaultPolicy is the API key's default model selector (alias).
	DefaultPolicy string
	// Scopes are the API key's scopes (ScopeChat, ScopeMCP); empty for a
	// browser session, which may do anything its user may.
	Scopes []string
}

// API key scopes. A key is minted with chat by default; mcp is explicit.
const (
	ScopeChat = "chat" // /v1 (chat completions, messages, models)
	ScopeMCP  = "mcp"  // /mcp (ws as an MCP server)
)

// KnownScopes lists the scopes a key may carry.
var KnownScopes = []string{ScopeChat, ScopeMCP}

// IsOwner reports admin rights.
func (p *Principal) IsOwner() bool { return p != nil && p.Role == "owner" }

// HasScope reports whether an API-key principal carries a scope. A
// session principal is not scoped and always qualifies.
func (p *Principal) HasScope(scope string) bool {
	if p == nil {
		return false
	}
	if !p.APIKeyID.Valid {
		return true
	}
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// CreateSession issues a session and sets the cookie.
func (s *Service) CreateSession(ctx context.Context, w http.ResponseWriter, r *http.Request, userID uuid.UUID) error {
	raw, hash := newToken()
	var ip *netip.Prefix
	if host := clientIP(r); host != "" {
		if addr, err := netip.ParseAddr(host); err == nil {
			p := netip.PrefixFrom(addr, addr.BitLen())
			ip = &p
		}
	}
	_, err := s.db.CreateSession(ctx, store.CreateSessionParams{
		UserID: userID, TokenHash: hash, ExpiresAt: time.Now().Add(SessionTTL),
		UserAgent: r.UserAgent(), Ip: ip,
	})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: raw, Path: "/", HttpOnly: true, Secure: s.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(SessionTTL.Seconds()),
	})
	return nil
}

// Authenticate resolves the session cookie or bearer API key on a request.
func (s *Service) Authenticate(ctx context.Context, r *http.Request) (*Principal, error) {
	if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
		return s.authenticateAPIKey(ctx, strings.TrimPrefix(ah, "Bearer "))
	}
	if xk := r.Header.Get("x-api-key"); xk != "" { // Anthropic-style clients
		return s.authenticateAPIKey(ctx, xk)
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return nil, ErrUnauthorized
	}
	row, err := s.db.GetSessionByTokenHash(ctx, hashToken(c.Value))
	if err != nil {
		return nil, ErrUnauthorized
	}
	return &Principal{UserID: row.UserID, Email: row.Email, DisplayName: row.DisplayName, Role: row.Role, SessionID: row.ID}, nil
}

func (s *Service) authenticateAPIKey(ctx context.Context, raw string) (*Principal, error) {
	if !strings.HasPrefix(raw, "ws_") {
		return nil, ErrUnauthorized
	}
	row, err := s.db.GetAPIKeyByHash(ctx, hashToken(raw))
	if err != nil {
		return nil, ErrUnauthorized
	}
	go func() { _ = s.db.TouchAPIKey(context.Background(), row.ID) }()
	return &Principal{UserID: row.UserID, Email: row.Email, Role: row.Role, APIKeyID: uuid.NullUUID{UUID: row.ID, Valid: true}, DefaultPolicy: row.DefaultPolicy, Scopes: row.Scopes}, nil
}

// Logout revokes the current session and clears the cookie.
func (s *Service) Logout(ctx context.Context, w http.ResponseWriter, p *Principal) error {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	if p != nil && p.SessionID != uuid.Nil {
		return s.db.RevokeSession(ctx, p.SessionID)
	}
	return nil
}

// CreateAPIKey mints a platform API key. Returns the raw key once.
func (s *Service) CreateAPIKey(ctx context.Context, userID uuid.UUID, name string, scopes []string, defaultPolicy string) (string, *store.ApiKey, error) {
	raw, hash := newToken()
	raw = "ws_" + raw
	if len(scopes) == 0 {
		scopes = []string{ScopeChat}
	}
	for _, sc := range scopes {
		if sc != ScopeChat && sc != ScopeMCP {
			return "", nil, fmt.Errorf("unknown scope %q (chat, mcp)", sc)
		}
	}
	if defaultPolicy == "" {
		defaultPolicy = "auto"
	}
	k, err := s.db.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		UserID: userID, Name: name, Prefix: raw[:11], KeyHash: hashToken(raw), Scopes: scopes, DefaultPolicy: defaultPolicy,
	})
	if err != nil {
		return "", nil, err
	}
	_ = hash
	return raw, &k, nil
}

// ---- WebAuthn ----

// waUser adapts a store user + passkeys to webauthn.User.
type waUser struct {
	u     store.User
	creds []webauthn.Credential
}

func (w *waUser) WebAuthnID() []byte                         { return w.u.ID[:] }
func (w *waUser) WebAuthnName() string                       { return w.u.Email }
func (w *waUser) WebAuthnDisplayName() string                { return w.u.DisplayName }
func (w *waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

func (s *Service) loadWAUser(ctx context.Context, u store.User) (*waUser, error) {
	rows, err := s.db.ListPasskeysByUser(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	wu := &waUser{u: u}
	for _, pk := range rows {
		wu.creds = append(wu.creds, credFromRow(pk))
	}
	return wu, nil
}

func credFromRow(pk store.Passkey) webauthn.Credential {
	var transports []protocol.AuthenticatorTransport
	for _, t := range pk.Transports {
		transports = append(transports, protocol.AuthenticatorTransport(t))
	}
	return webauthn.Credential{
		ID: pk.CredentialID, PublicKey: pk.PublicKey, AttestationType: pk.AttestationType, Transport: transports,
		Flags:         webauthn.CredentialFlags{BackupEligible: pk.BackupEligible, BackupState: pk.BackupState},
		Authenticator: webauthn.Authenticator{AAGUID: pk.Aaguid, SignCount: uint32(pk.SignCount)},
	}
}

// BeginRegistration starts a passkey registration ceremony for a user.
// Returns the options JSON for navigator.credentials.create and a ceremony id.
func (s *Service) BeginRegistration(ctx context.Context, userID uuid.UUID) (json.RawMessage, uuid.UUID, error) {
	if s.wa == nil {
		return nil, uuid.Nil, ErrPasskeysUnavailable
	}
	u, err := s.db.GetUserByID(ctx, userID)
	if err != nil {
		return nil, uuid.Nil, err
	}
	wu, err := s.loadWAUser(ctx, u)
	if err != nil {
		return nil, uuid.Nil, err
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range wu.creds {
		exclude = append(exclude, c.Descriptor())
	}
	opts, sd, err := s.wa.BeginRegistration(wu, webauthn.WithExclusions(exclude))
	if err != nil {
		return nil, uuid.Nil, err
	}
	cid, err := s.saveCeremony(ctx, &u.ID, "register", sd)
	if err != nil {
		return nil, uuid.Nil, err
	}
	b, _ := json.Marshal(opts)
	return b, cid, nil
}

// FinishRegistration completes the ceremony with the browser's response.
func (s *Service) FinishRegistration(ctx context.Context, ceremonyID uuid.UUID, name string, r *http.Request) error {
	if s.wa == nil {
		return ErrPasskeysUnavailable
	}
	sd, userID, err := s.loadCeremony(ctx, ceremonyID, "register")
	if err != nil {
		return err
	}
	u, err := s.db.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	wu, err := s.loadWAUser(ctx, u)
	if err != nil {
		return err
	}
	cred, err := s.wa.FinishRegistration(wu, *sd, r)
	if err != nil {
		return fmt.Errorf("auth: finish registration: %w", err)
	}
	var transports []string
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	if name == "" {
		name = "Passkey"
	}
	_, err = s.db.CreatePasskey(ctx, store.CreatePasskeyParams{
		UserID: u.ID, CredentialID: cred.ID, PublicKey: cred.PublicKey, AttestationType: cred.AttestationType,
		Transports: transports, Aaguid: cred.Authenticator.AAGUID, SignCount: int64(cred.Authenticator.SignCount),
		BackupEligible: cred.Flags.BackupEligible, BackupState: cred.Flags.BackupState, Name: name,
	})
	return err
}

// BeginLogin starts a discoverable-credential (usernameless) login.
func (s *Service) BeginLogin(ctx context.Context) (json.RawMessage, uuid.UUID, error) {
	if s.wa == nil {
		return nil, uuid.Nil, ErrPasskeysUnavailable
	}
	opts, sd, err := s.wa.BeginDiscoverableLogin()
	if err != nil {
		return nil, uuid.Nil, err
	}
	cid, err := s.saveCeremony(ctx, nil, "login", sd)
	if err != nil {
		return nil, uuid.Nil, err
	}
	b, _ := json.Marshal(opts)
	return b, cid, nil
}

// FinishLogin validates the assertion and returns the user.
func (s *Service) FinishLogin(ctx context.Context, ceremonyID uuid.UUID, r *http.Request) (*store.User, error) {
	if s.wa == nil {
		return nil, ErrPasskeysUnavailable
	}
	sd, _, err := s.loadCeremony(ctx, ceremonyID, "login")
	if err != nil {
		return nil, err
	}
	var found store.User
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		pk, err := s.db.GetPasskeyByCredentialID(ctx, rawID)
		if err != nil {
			return nil, ErrUnauthorized
		}
		u, err := s.db.GetUserByID(ctx, pk.UserID)
		if err != nil {
			return nil, ErrUnauthorized
		}
		if subtle.ConstantTimeCompare(userHandle, u.ID[:]) != 1 {
			return nil, ErrUnauthorized
		}
		found = u
		return s.loadWAUser(ctx, u)
	}
	cred, err := s.wa.FinishDiscoverableLogin(handler, *sd, r)
	if err != nil {
		return nil, fmt.Errorf("auth: finish login: %w", err)
	}
	if pk, err := s.db.GetPasskeyByCredentialID(ctx, cred.ID); err == nil {
		_ = s.db.UpdatePasskeyAfterLogin(ctx, store.UpdatePasskeyAfterLoginParams{
			ID: pk.ID, SignCount: int64(cred.Authenticator.SignCount), BackupState: cred.Flags.BackupState,
		})
	}
	return &found, nil
}

func (s *Service) saveCeremony(ctx context.Context, userID *uuid.UUID, kind string, sd *webauthn.SessionData) (uuid.UUID, error) {
	b, err := json.Marshal(sd)
	if err != nil {
		return uuid.Nil, err
	}
	var uid uuid.NullUUID
	if userID != nil {
		uid = uuid.NullUUID{UUID: *userID, Valid: true}
	}
	row, err := s.db.CreateWebAuthnSession(ctx, store.CreateWebAuthnSessionParams{
		UserID: uid, Kind: kind, Data: b, ExpiresAt: time.Now().Add(WebAuthnCereTTL),
	})
	if err != nil {
		return uuid.Nil, err
	}
	return row.ID, nil
}

func (s *Service) loadCeremony(ctx context.Context, id uuid.UUID, kind string) (*webauthn.SessionData, uuid.UUID, error) {
	row, err := s.db.GetWebAuthnSession(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, uuid.Nil, ErrInvalidToken
		}
		return nil, uuid.Nil, err
	}
	_ = s.db.DeleteWebAuthnSession(ctx, id) // single use
	if row.Kind != kind {
		return nil, uuid.Nil, ErrInvalidToken
	}
	var sd webauthn.SessionData
	if err := json.Unmarshal(row.Data, &sd); err != nil {
		return nil, uuid.Nil, err
	}
	return &sd, row.UserID.UUID, nil
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if cf := r.Header.Get("CF-Connecting-IP"); cf != "" {
		return cf
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

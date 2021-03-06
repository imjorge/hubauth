package google

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/flynn/hubauth/pkg/clog"
	"github.com/flynn/hubauth/pkg/hubauth"
	"github.com/flynn/hubauth/pkg/rp"
	"go.opencensus.io/plugin/ochttp"
	"go.opencensus.io/trace"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

func New(clientID, clientSecret, redirectURL string) rp.AuthService {
	return &service{
		hc: &http.Client{
			Transport: &ochttp.Transport{},
		},
		conf: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     google.Endpoint,
			RedirectURL:  redirectURL,
			Scopes:       []string{"openid", "profile", "email"},
		},
	}
}

type service struct {
	hc   *http.Client
	conf *oauth2.Config
}

const nonceRandom = 8

func genNonce() string {
	random := make([]byte, nonceRandom)
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		// this should be unreachable
		panic(err)
	}
	return strings.TrimRight(base64.URLEncoding.EncodeToString(random), "=")
}

func (s *service) Redirect(ctx context.Context) (*rp.AuthCodeRedirect, error) {
	nonce := genNonce()
	u := s.conf.AuthCodeURL(nonce, oauth2.SetAuthURLParam("nonce", nonce), oauth2.SetAuthURLParam("hd", "*"))
	return &rp.AuthCodeRedirect{
		URL:   u,
		State: nonce,
	}, nil
}

const codeInvalid = "invalid_request"

func (s *service) Exchange(ctx context.Context, r *rp.RedirectResult) (*rp.Token, error) {
	ctx, span := trace.StartSpan(ctx, "google.ExchangeCode")
	defer span.End()

	if e := r.Params.Get("error"); e != "" {
		return nil, hubauth.OAuthError{Description: r.Params.Get("error_description"), Code: e}
	}

	nonce := r.Params.Get("state")
	if nonce == "" {
		return nil, hubauth.OAuthError{Description: "missing state", Code: codeInvalid}
	}
	if r.State != nonce {
		return nil, hubauth.OAuthError{Description: "state mismatch", Code: codeInvalid}
	}

	code := r.Params.Get("code")
	if code == "" {
		return nil, hubauth.OAuthError{Description: "missing code", Code: codeInvalid}
	}

	ctx = context.WithValue(ctx, oauth2.HTTPClient, s.hc)
	t, err := s.conf.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}

	id, ok := t.Extra("id_token").(string)
	if !ok || id == "" {
		return nil, hubauth.OAuthError{Description: "missing id_token", Code: codeInvalid}
	}
	clog.Set(ctx, zap.String("google_id_token", id))
	splitJWT := strings.SplitN(id, ".", 3)
	if len(splitJWT) < 3 {
		return nil, hubauth.OAuthError{Description: "invalid id_token", Code: codeInvalid}
	}
	// no need to check the signature, as we just got it over TLS from Google
	if m := len(splitJWT[1]) % 4; m != 0 {
		splitJWT[1] += strings.Repeat("=", 4-m)
	}
	idJSON, err := base64.URLEncoding.DecodeString(splitJWT[1])
	if err != nil {
		clog.Set(ctx, zap.NamedError("decode_err", err))
		return nil, hubauth.OAuthError{Description: "invalid id_token encoding", Code: codeInvalid}
	}

	var idt idToken
	if err := json.Unmarshal(idJSON, &idt); err != nil {
		clog.Set(ctx, zap.NamedError("decode_err", err))
		return nil, hubauth.OAuthError{Description: "invalid id_token json", Code: codeInvalid}
	}
	if idt.Nonce != nonce {
		return nil, hubauth.OAuthError{Description: "id_token missing nonce", Code: codeInvalid}
	}
	if !idt.EmailVerified || idt.Email == "" || idt.Sub == "" {
		return nil, hubauth.OAuthError{Description: "id_token missing user", Code: codeInvalid}
	}

	span.AddAttributes(
		trace.StringAttribute("token_sub", idt.Sub),
		trace.StringAttribute("token_email", idt.Email),
	)
	return &rp.Token{
		Token:   t,
		UserID:  idt.Sub,
		Email:   idt.Email,
		Name:    idt.Name,
		Picture: idt.Picture,
	}, nil
}

type idToken struct {
	Nonce         string
	Sub           string
	Email         string
	EmailVerified bool `json:"email_verified"`
	Name          string
	Picture       string
}

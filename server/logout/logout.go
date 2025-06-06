package logout

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	log "github.com/sirupsen/logrus"

	"github.com/argoproj/argo-cd/v3/common"
	httputil "github.com/argoproj/argo-cd/v3/util/http"
	jwtutil "github.com/argoproj/argo-cd/v3/util/jwt"
	"github.com/argoproj/argo-cd/v3/util/session"
	"github.com/argoproj/argo-cd/v3/util/settings"
)

// NewHandler creates handler serving to do api/logout endpoint
func NewHandler(settingsMrg *settings.SettingsManager, sessionMgr *session.SessionManager, rootPath, baseHRef string) *Handler {
	return &Handler{
		settingsMgr: settingsMrg,
		rootPath:    rootPath,
		baseHRef:    baseHRef,
		verifyToken: sessionMgr.VerifyToken,
		revokeToken: sessionMgr.RevokeToken,
	}
}

type Handler struct {
	settingsMgr *settings.SettingsManager
	rootPath    string
	verifyToken func(tokenString string) (jwt.Claims, string, error)
	revokeToken func(ctx context.Context, id string, expiringAt time.Duration) error
	baseHRef    string
}

var (
	tokenPattern             = regexp.MustCompile(`{{token}}`)
	logoutRedirectURLPattern = regexp.MustCompile(`{{logoutRedirectURL}}`)
)

func constructLogoutURL(logoutURL, token, logoutRedirectURL string) string {
	constructedLogoutURL := tokenPattern.ReplaceAllString(logoutURL, token)
	return logoutRedirectURLPattern.ReplaceAllString(constructedLogoutURL, logoutRedirectURL)
}

// ServeHTTP is the logout handler for ArgoCD and constructs OIDC logout URL and redirects to it for OIDC issued sessions,
// and redirects user to '/login' for argocd issued sessions
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var tokenString string
	var oidcConfig *settings.OIDCConfig

	argoCDSettings, err := h.settingsMgr.GetSettings()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		http.Error(w, "Failed to retrieve argoCD settings: "+err.Error(), http.StatusInternalServerError)
		return
	}

	argoURL, err := argoCDSettings.ArgoURLForRequest(r)
	if err != nil {
		log.Warnf("unable to find ArgoCD URL from config: %v", err)
	}
	if argoURL == "" {
		// golang does not provide any easy way to determine scheme of current request
		// so redirecting ot http which will auto-redirect too https if necessary
		host := strings.TrimRight(r.Host, "/")
		argoURL = "http://" + host + "/" + strings.TrimRight(strings.TrimLeft(h.rootPath, "/"), "/")
	}

	logoutRedirectURL := strings.TrimRight(strings.TrimLeft(argoURL, "/"), "/")

	cookies := r.Cookies()
	tokenString, err = httputil.JoinCookies(common.AuthCookieName, cookies)
	if tokenString == "" || err != nil {
		w.WriteHeader(http.StatusBadRequest)
		http.Error(w, "Failed to retrieve ArgoCD auth token: "+err.Error(), http.StatusBadRequest)
		return
	}

	for _, cookie := range cookies {
		if !strings.HasPrefix(cookie.Name, common.AuthCookieName) {
			continue
		}

		argocdCookie := http.Cookie{
			Name:  cookie.Name,
			Value: "",
		}

		argocdCookie.Path = "/" + strings.TrimRight(strings.TrimLeft(h.baseHRef, "/"), "/")
		w.Header().Add("Set-Cookie", argocdCookie.String())
	}

	claims, _, err := h.verifyToken(tokenString)
	if err != nil {
		http.Redirect(w, r, logoutRedirectURL, http.StatusSeeOther)
		return
	}

	mapClaims, err := jwtutil.MapClaims(claims)
	if err != nil {
		http.Redirect(w, r, logoutRedirectURL, http.StatusSeeOther)
		return
	}

	issuer := jwtutil.StringField(mapClaims, "iss")
	id := jwtutil.StringField(mapClaims, "jti")
	if exp, err := jwtutil.ExpirationTime(mapClaims); err == nil && id != "" {
		if err := h.revokeToken(context.Background(), id, time.Until(exp)); err != nil {
			log.Warnf("failed to invalidate token '%s': %v", id, err)
		}
	}

	if argoCDSettings.OIDCConfig() == nil || argoCDSettings.OIDCConfig().LogoutURL == "" || issuer == session.SessionManagerClaimsIssuer {
		http.Redirect(w, r, logoutRedirectURL, http.StatusSeeOther)
	} else {
		oidcConfig = argoCDSettings.OIDCConfig()
		logoutURL := constructLogoutURL(oidcConfig.LogoutURL, tokenString, logoutRedirectURL)
		http.Redirect(w, r, logoutURL, http.StatusSeeOther)
	}
}

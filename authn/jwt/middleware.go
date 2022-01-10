package jwt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-kit/kit/endpoint"
	"github.com/golang-jwt/jwt/v4"
	"github.com/jdotw/go-utils/log"
	"github.com/jdotw/go-utils/tracing"
	"github.com/opentracing/opentracing-go"
	"go.uber.org/zap"
)

type contextKey string

const (
	// JWTContextKey holds the key used to store a JWT in the context.
	JWTContextKey contextKey = "JWTToken"

	// JWTTokenContextKey is an alias for JWTContextKey.
	//
	// Deprecated: prefer JWTContextKey.
	JWTTokenContextKey = JWTContextKey

	// JWTClaimsContextKey holds the key used to store the JWT Claims in the
	// context.
	JWTClaimsContextKey       contextKey = "JWTClaims"
	JWTDecodedTokenContextKey contextKey = "JWTDecodedToken"
)

var (
	// ErrTokenContextMissing denotes a token was not passed into the parsing
	// middleware's context.
	ErrTokenContextMissing = errors.New("JWT not present")

	// ErrTokenInvalid denotes a token was not able to be validated.
	ErrTokenInvalid = errors.New("JWT was invalid")

	// ErrTokenExpired denotes a token's expire header (exp) has since passed.
	ErrTokenExpired = errors.New("JWT is expired")

	// ErrTokenMalformed denotes a token was not formatted as a JWT.
	ErrTokenMalformed = errors.New("JWT is malformed")

	// ErrTokenNotActive denotes a token's not before header (nbf) is in the
	// future.
	ErrTokenNotActive = errors.New("token is not valid yet")

	// ErrUnexpectedSigningMethod denotes a token was signed with an unexpected
	// signing method.
	ErrUnexpectedSigningMethod = errors.New("unexpected signing method")
)

type Jwks struct {
	Keys []JSONWebKeys `json:"keys"`
}

type JSONWebKeys struct {
	Kty string   `json:"kty"`
	Kid string   `json:"kid"`
	Use string   `json:"use"`
	N   string   `json:"n"`
	E   string   `json:"e"`
	X5c []string `json:"x5c"`
}

// NewSigner creates a new JWT generating middleware, specifying key ID,
// signing string, signing method and the claims you would like it to contain.
// Tokens are signed with a Key ID header (kid) which is useful for determining
// the key to use for parsing. Particularly useful for clients.
func NewSigner(kid string, key []byte, method jwt.SigningMethod, claims jwt.Claims) endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, request interface{}) (response interface{}, err error) {
			token := jwt.NewWithClaims(method, claims)
			token.Header["kid"] = kid

			// Sign and get the complete encoded token as a string using the secret
			tokenString, err := token.SignedString(key)
			if err != nil {
				return nil, err
			}
			ctx = context.WithValue(ctx, JWTContextKey, tokenString)

			return next(ctx, request)
		}
	}
}

// ClaimsFactory is a factory for jwt.Claims.
// Useful in NewParser middleware.
type ClaimsFactory func() jwt.Claims

// MapClaimsFactory is a ClaimsFactory that returns
// an empty jwt.MapClaims.
func MapClaimsFactory() jwt.Claims {
	return jwt.MapClaims{}
}

// StandardClaimsFactory is a ClaimsFactory that returns
// an empty jwt.StandardClaims.
func StandardClaimsFactory() jwt.Claims {
	return &jwt.StandardClaims{}
}

//
// jdotw/go-utils Additions
//

type Authenticator struct {
	logger log.Factory
	tracer opentracing.Tracer

	jwks    *Jwks
	jwksURL string
}

func NewAuthenticator(logger log.Factory, tracer opentracing.Tracer, jwksURL string) Authenticator {
	a := Authenticator{
		logger:  logger,
		tracer:  tracer,
		jwksURL: jwksURL,
	}

	jwks, err := a.getJWKS()
	if err != nil {
		a.logger.Bg().Fatal(err.Error())
	}
	a.jwks = jwks

	return a
}

func (a *Authenticator) getJWKS() (*Jwks, error) {
	resp, err := http.Get(a.jwksURL)

	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var jwks = Jwks{}
	err = json.NewDecoder(resp.Body).Decode(&jwks)
	if err != nil {
		return nil, err
	}

	return &jwks, nil
}

// NewMiddleware creates an Endpoint middleware
// that parses and validates the JWT token added to the ctx
// by the transport layers.
// The signing string is read from the JWT_SIGNATURE env var
func (a *Authenticator) NewMiddleware() endpoint.Middleware {
	kf := func(token *jwt.Token) (interface{}, error) {
		// Find matching Key in JWKS
		var cert string
		for k := range a.jwks.Keys {
			if token.Header["kid"] == a.jwks.Keys[k].Kid {
				cert = "-----BEGIN CERTIFICATE-----\n" + a.jwks.Keys[k].X5c[0] + "\n-----END CERTIFICATE-----"
			}
		}
		if len(cert) == 0 {
			err := errors.New("invalid key id")
			return token, err
		}

		// Return Public Key
		result, _ := jwt.ParseRSAPublicKeyFromPEM([]byte(cert))
		return result, nil
	}
	method := jwt.SigningMethodRS256
	return newParser(kf, method, MapClaimsFactory, *a)
}

func extractTokenFromContext(ctx context.Context) (*string, error) {
	// tokenString is stored in the context from the transport handlers.
	tokenString, ok := ctx.Value(JWTContextKey).(string)
	if !ok {
		return nil, ErrTokenContextMissing
	}
	return &tokenString, nil
}

func parseTokenString(ctx context.Context, tokenString string, expectedSigningMethod jwt.SigningMethod, newClaims ClaimsFactory, keyFunc jwt.Keyfunc) (*jwt.Token, error) {
	// Parse takes the token string and a function for looking up the
	// key. The latter is especially useful if you use multiple keys
	// for your application.  The standard is to use 'kid' in the head
	// of the token to identify which key to use, but the parsed token
	// (head and claims) is provided to the callback, providing
	// flexibility.
	token, err := jwt.ParseWithClaims(tokenString, newClaims(), func(token *jwt.Token) (interface{}, error) {
		// Don't forget to validate the alg is what you expect:
		if token.Method != expectedSigningMethod {
			return nil, ErrUnexpectedSigningMethod
		}
		return keyFunc(token)
	})
	return token, err
}

// newParser creates a new JWT parsing middleware, specifying a
// jwt.Keyfunc interface, the signing method and the claims type to be used. NewParser
// adds the resulting claims to endpoint context or returns error on invalid token.
// Particularly useful for servers.
func newParser(keyFunc jwt.Keyfunc, method jwt.SigningMethod, newClaims ClaimsFactory, authn Authenticator) endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, request interface{}) (response interface{}, err error) {

			ctx, span := tracing.NewChildSpanAndContext(ctx, authn.tracer, "ParseJWT")

			tokenString, err := extractTokenFromContext(ctx)
			if err != nil {
				authn.logger.For(ctx).Error("Failed to extract JWT token from context", zap.Error(err))
				span.Finish()
				return nil, err
			}

			token, err := parseTokenString(ctx, *tokenString, method, newClaims, keyFunc)
			if err != nil {
				if e, ok := err.(*jwt.ValidationError); ok {
					switch {
					case e.Errors&jwt.ValidationErrorMalformed != 0:
						// Token is malformed
						authn.logger.For(ctx).Error("Malformed JWT", zap.Error(err))
						span.Finish()
						return nil, ErrTokenMalformed
					case e.Errors&jwt.ValidationErrorExpired != 0:
						// Token is expired
						authn.logger.For(ctx).Error("Expired JWT", zap.Error(err))
						span.Finish()
						return nil, ErrTokenExpired
					case e.Errors&jwt.ValidationErrorNotValidYet != 0:
						// Token is not active yet
						authn.logger.For(ctx).Error("JWT Not Yet Valid", zap.Error(err))
						span.Finish()
						return nil, ErrTokenNotActive
					case e.Inner != nil:
						// report e.Inner
						authn.logger.For(ctx).Error("JWT Inner Error", zap.Error(e.Inner))
						span.Finish()
						return nil, e.Inner
					}
					// We have a ValidationError but have no specific Go kit error for it.
					// Fall through to return original error.
					authn.logger.For(ctx).Error("Other JWT Validation Error", zap.Error(err))
				} else {
					authn.logger.For(ctx).Error("Unknown JWT Error", zap.Error(err))
				}
				span.Finish()
				return nil, err
			}

			if !token.Valid {
				authn.logger.For(ctx).Error("Invalid JWT")
				span.Finish()
				return nil, ErrTokenInvalid
			}

			ctx = context.WithValue(ctx, JWTDecodedTokenContextKey, token)
			ctx = context.WithValue(ctx, JWTClaimsContextKey, token.Claims)

			span.Finish()

			return next(ctx, request)
		}
	}
}

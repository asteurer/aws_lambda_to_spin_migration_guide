package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	spinhttp "github.com/fermyon/spin/sdk/go/v2/http"
	"github.com/fermyon/spin/sdk/go/v2/variables"
)

func init() {
	spinhttp.Handle(func(w http.ResponseWriter, r *http.Request) {
		err := initConfig()
		if err != nil {
			fmt.Println("Failed to read configuration:", err)
			http.Error(w, "Internal Server Error Occurred", http.StatusInternalServerError)
			return
		}

		// Read the request body
		payloadBytes, err := io.ReadAll(r.Body)
		if err != nil {
			fmt.Println("Error reading request body:", err)
			http.Error(w, "Internal Server Error Occurred", http.StatusInternalServerError)
			return
		}
		r.Body.Close()

		uriPath := r.Header.Get("uri-path")
		// If necessary, add query parameters: 'queryParams[key] = value'
		queryParams := make(map[string]string)
		// If necessary, add extra headers: 'headers[key] = value'
		headers := make(map[string]string)

		req, err := buildAwsHttpRequest(r.Method, config.host, uriPath, queryParams, headers, payloadBytes)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to create outbound http request: %s", err.Error()), http.StatusInternalServerError)
			return
		}

		resp, err := sendHttpRequest(req)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to execute outbound http request: %s", err.Error()), http.StatusInternalServerError)
			return
		}
		if resp.StatusCode != http.StatusOK {
			http.Error(w, fmt.Sprintf("response from outbound http request is not OK %v", resp.Status), http.StatusInternalServerError)
			return
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to read outbound http response: %s", err.Error()), http.StatusInternalServerError)
			return
		}

		if len(body) == 0 {
			http.Error(w, fmt.Sprintf("outbound http response was empty\n"), http.StatusInternalServerError)
			return
		}

		// Set the status code from the response
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
	})
}

func main() {
	/*
	 *	In Spin environment, the handler function has to be wired up during the init function
	 *  because Spin will call into the wasi export named spin_http_handle_http_request
	 *  so we need to set the default handler during the init function which is called
	 *  before the exported spin_http_handle_http_request function in our SDK
	 *  spinhttp.Handle: https://github.com/fermyon/spin-go-sdk/blob/48ddef7a261725f771f987323b213b0696c655ef/http/http.go#L93
	 *  internals.go:    https://github.com/fermyon/spin-go-sdk/blob/48ddef7a261725f771f987323b213b0696c655ef/http/internals.go#L16
	 */
}

type AWSConfig struct {
	accessKeyID     string
	secretAccessKey string
	sessionToken    string
	region          string
	service         string
	host            string
}

var config AWSConfig

func initConfig() error {
	accessKeyID, err := variables.Get("aws_access_key_id")
	if err != nil {
		return err
	}
	secretAccessKey, err := variables.Get("aws_secret_access_key")
	if err != nil {
		return err
	}
	sessionToken, err := variables.Get("aws_session_token")
	if err != nil {
		return err
	}
	region, err := variables.Get("aws_default_region")
	if err != nil {
		return err
	}
	service, err := variables.Get("aws_service")
	if err != nil {
		return err
	}
	host, err := variables.Get("aws_host")
	if err != nil {
		return err
	}

	config = AWSConfig{
		accessKeyID:     accessKeyID,
		secretAccessKey: secretAccessKey,
		sessionToken:    sessionToken,
		region:          region,
		service:         service,
		host:            host,
	}

	return nil
}

func sendHttpRequest(req *http.Request) (*http.Response, error) {
	sender, _ := variables.Get("sender")
	switch sender {
	case "http.DefaultClient.Do":
		return http.DefaultClient.Do(req)
	case "":
		fallthrough
	case "spinhttp.Send":
		fallthrough
	default:
		return spinhttp.Send(req)
	}
}

func buildHeaderStrings(headers http.Header, queryParams map[string]string) (string, string, string) {
	// Building canonical and signed headers
	canonicalHeaders := ""
	signedHeaders := ""
	headerKeys := make([]string, 0, len(headers))
	for key := range headers {
		headerKeys = append(headerKeys, key)
	}
	sort.Strings(headerKeys)

	for _, key := range headerKeys {
		// The header keys are capitalized when added to the request, so the header strings are created with the lowerCaseKey, and the header values are looked up by the upper case key.
		lowerCaseKey := strings.ToLower(key)
		canonicalHeaders += fmt.Sprintf("%s:%s\n", lowerCaseKey, headers.Get(key))
		if signedHeaders == "" {
			signedHeaders += lowerCaseKey
		} else {
			signedHeaders += fmt.Sprintf(";%s", lowerCaseKey)
		}
	}

	// Building query params
	queryKeys := make([]string, 0, len(queryParams))
	for key := range queryParams {
		queryKeys = append(queryKeys, key)
	}
	sort.Strings(queryKeys)
	var canonicalQueryStringArray []string
	for _, key := range queryKeys {
		canonicalQueryStringArray = append(canonicalQueryStringArray, fmt.Sprintf("%s=%s"), key, queryParams[key])
	}
	canonicalQueryString := strings.Join(canonicalQueryStringArray, "&")

	return canonicalHeaders, signedHeaders, canonicalQueryString
}

func buildAwsHttpRequest(httpVerb, host, uriPath string, queryParams, headers map[string]string, payload []byte) (*http.Request, error) {
	if uriPath == "" || uriPath[0] != '/' {
		uriPath = "/" + uriPath
	}

	destinationUrl := fmt.Sprintf("http://%s%s", host, uriPath)

	req, err := http.NewRequest(httpVerb, destinationUrl, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create new request: %w", err)
	}

	now := time.Now().UTC()

	// Adding extra headers
	for key, value := range headers {
		// Ensuring that the header keys are lowercase for proper signing.
		req.Header.Set(strings.ToLower(key), value)
	}

	payloadHash := hash(payload)
	if payloadHash == "" {
		return nil, fmt.Errorf("failed to generate hash for payload")
	}

	req.Header.Set("host", host)
	req.Header.Set("x-amz-date", now.Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("content-length", fmt.Sprintf("%d", len(payload)))
	// sessionToken is optional
	if config.sessionToken != "" {
		req.Header.Set("x-amz-security-token", config.sessionToken)
	}

	canonicalHeaders, signedHeaders, canonicalQueryString := buildHeaderStrings(req.Header, queryParams)
	canonicalRequest := getCanonicalRequest(httpVerb, uriPath, canonicalQueryString, canonicalHeaders, signedHeaders, req.Header.Get("x-amz-content-sha256"))

	req.Header.Set("authorization", getAuthorizationHeader(now, canonicalRequest, signedHeaders))

	delete(headers, "host")

	return req, nil
}

func getAuthorizationHeader(now time.Time, canonicalRequest, signedHeaders string) string {

	// Create the string to sign
	stringToSign := getStringToSign(canonicalRequest, now)

	// Calculate the signature
	signature := getSignature(stringToSign, now)

	// Create the authorization header
	authorizationHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s",
		config.accessKeyID, now.Format("20060102"), config.region, config.service, signedHeaders, signature)

	return authorizationHeader
}

func getCanonicalRequest(httpVerb, canonicalUri, canonicalQueryString, canonicalHeaders, signedHeaders, unsignedPayloadHash string) string {
	return strings.Join([]string{httpVerb, canonicalUri, canonicalQueryString, canonicalHeaders, signedHeaders, unsignedPayloadHash}, "\n")
}

func getStringToSign(canonicalRequest string, now time.Time) string {
	// Create the string to sign
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s/%s/%s/aws4_request\n%s",
		now.Format("20060102T150405Z"), now.Format("20060102"), config.region, config.service, hash([]byte(canonicalRequest)))

	return stringToSign
}

func getSignature(stringToSign string, now time.Time) string {
	// Create the signing key
	dateKey := hmacSHA256([]byte("AWS4"+config.secretAccessKey), []byte(now.Format("20060102")))
	regionKey := hmacSHA256(dateKey, []byte(config.region))
	serviceKey := hmacSHA256(regionKey, []byte(config.service))
	signingKey := hmacSHA256(serviceKey, []byte("aws4_request"))

	// Calculate the signature
	signature := hmacSHA256(signingKey, []byte(stringToSign))

	return hex.EncodeToString(signature)
}

func hmacSHA256(key []byte, data []byte) []byte {
	hash := hmac.New(sha256.New, key)
	hash.Write(data)
	return hash.Sum(nil)
}

func hash(payload []byte) string {
	hash := sha256.New()
	hash.Write(payload)
	return hex.EncodeToString(hash.Sum(nil))
}

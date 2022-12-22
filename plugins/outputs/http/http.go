//go:generate ../../../tools/readme_config_includer/generator
package http

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"time"

	awsV2 "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/influxdata/telegraf"
	internalaws "github.com/influxdata/telegraf/config/aws"
	"github.com/influxdata/telegraf/internal"
	httpconfig "github.com/influxdata/telegraf/plugins/common/http"
	"github.com/influxdata/telegraf/plugins/outputs"
	"github.com/influxdata/telegraf/plugins/serializers"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
)

// DO NOT REMOVE THE NEXT TWO LINES! This is required to embed the sampleConfig data.
//
//go:embed sample.conf
var sampleConfig string

const (
	maxErrMsgLen = 1024
	defaultURL   = "http://127.0.0.1:8080/telegraf"
)

const (
	defaultContentType    = "text/plain; charset=utf-8"
	defaultMethod         = http.MethodPost
	defaultUseBatchFormat = true
)

type HTTP struct {
	URL                     string            `toml:"url"`
	Method                  string            `toml:"method"`
	Username                string            `toml:"username"`
	Password                string            `toml:"password"`
	Headers                 map[string]string `toml:"headers"`
	TagHeaders              map[string]string `toml:"tag_headers"`
	ContentEncoding         string            `toml:"content_encoding"`
	UseBatchFormat          bool              `toml:"use_batch_format"`
	AwsService              string            `toml:"aws_service"`
	NonRetryableStatusCodes []int             `toml:"non_retryable_statuscodes"`
	httpconfig.HTTPClientConfig
	Log telegraf.Logger `toml:"-"`

	client     *http.Client
	serializer serializers.Serializer

	awsCfg *awsV2.Config
	internalaws.CredentialConfig

	// Google API Auth
	CredentialsFile string `toml:"google_application_credentials"`
	oauth2Token     *oauth2.Token
}

type Headers map[string]string

func (*HTTP) SampleConfig() string {
	return sampleConfig
}

func (h *HTTP) SetSerializer(serializer serializers.Serializer) {
	h.serializer = serializer
}

func (h *HTTP) Connect() error {
	if h.AwsService != "" {
		cfg, err := h.CredentialConfig.Credentials()
		if err == nil {
			h.awsCfg = &cfg
		}
	}

	if h.Method == "" {
		h.Method = http.MethodPost
	}
	h.Method = strings.ToUpper(h.Method)
	if h.Method != http.MethodPost && h.Method != http.MethodPut {
		return fmt.Errorf("invalid method [%s] %s", h.URL, h.Method)
	}

	ctx := context.Background()
	client, err := h.HTTPClientConfig.CreateClient(ctx, h.Log)
	if err != nil {
		return err
	}

	h.client = client

	return nil
}

func (h *HTTP) Close() error {
	return nil
}

func (h *HTTP) Write(metrics []telegraf.Metric) error {
	var reqBody []byte
	var headers = Headers{}

	if h.UseBatchFormat {
		if len(h.TagHeaders) > 0 {
			headersmap := make(map[uint64]Headers)
			metricsmap := make(map[uint64][]telegraf.Metric)

			for _, metric := range metrics {
				id := HashIDFromTagHeaders(metric, h.TagHeaders)

				if _, ok := headersmap[id]; !ok {
					headersmap[id] = h.createHeaders(metric)
				}

				// Avoid modifying the metric in case we need to retry the request.
				metric = metric.Copy()
				metric.Accept()
				for t := range h.TagHeaders {
					metric.RemoveTag(t)
				}

				metricsmap[id] = append(metricsmap[id], metric)
			}

			for id, headers := range headersmap {
				var err error

				reqBody, err = h.serializer.SerializeBatch(metricsmap[id])
				if err != nil {
					return err
				}

				if err := h.writeMetric(reqBody, headers); err != nil {
					return err
				}
			}

			return nil

		} else {
			var err error

			reqBody, err = h.serializer.SerializeBatch(metrics)
			if err != nil {
				return err
			}

			return h.writeMetric(reqBody, h.Headers)
		}
	}

	for _, metric := range metrics {
		var err error

		headers = h.createHeaders(metric)
		// Avoid modifying the metric in case we need to retry the request.
		metric = metric.Copy()
		metric.Accept()
		for t := range h.TagHeaders {
			metric.RemoveTag(t)
		}

		reqBody, err = h.serializer.Serialize(metric)
		if err != nil {
			return err
		}

		if err := h.writeMetric(reqBody, headers); err != nil {
			return err
		}
	}
	return nil
}

func (h *HTTP) writeMetric(reqBody []byte, headers Headers) error {
	var reqBodyBuffer io.Reader = bytes.NewBuffer(reqBody)

	var err error
	if h.ContentEncoding == "gzip" {
		rc, err := internal.CompressWithGzip(reqBodyBuffer)
		if err != nil {
			return err
		}
		defer rc.Close()
		reqBodyBuffer = rc
	}

	var payloadHash *string
	if h.awsCfg != nil {
		// We need a local copy of the full buffer, the signature scheme requires a sha256 of the request body.
		buf := new(bytes.Buffer)
		_, err = io.Copy(buf, reqBodyBuffer)
		if err != nil {
			return err
		}

		sum := sha256.Sum256(buf.Bytes())
		reqBodyBuffer = buf

		// sha256 is hex encoded
		hash := fmt.Sprintf("%x", sum)
		payloadHash = &hash
	}

	req, err := http.NewRequest(h.Method, h.URL, reqBodyBuffer)
	if err != nil {
		return err
	}

	if h.awsCfg != nil {
		signer := v4.NewSigner()
		ctx := context.Background()

		credentials, err := h.awsCfg.Credentials.Retrieve(ctx)
		if err != nil {
			return err
		}

		err = signer.SignHTTP(ctx, credentials, req, *payloadHash, h.AwsService, h.Region, time.Now().UTC())
		if err != nil {
			return err
		}
	}

	if h.Username != "" || h.Password != "" {
		req.SetBasicAuth(h.Username, h.Password)
	}

	// google api auth
	if h.CredentialsFile != "" {
		token, err := h.getAccessToken(context.Background(), h.URL)
		if err != nil {
			return err
		}
		token.SetAuthHeader(req)
	}

	req.Header.Set("User-Agent", internal.ProductToken())
	req.Header.Set("Content-Type", defaultContentType)
	if h.ContentEncoding == "gzip" {
		req.Header.Set("Content-Encoding", "gzip")
	}
	for k, v := range headers {
		if strings.ToLower(k) == "host" {
			req.Host = v
		}
		req.Header.Set(k, v)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		for _, nonRetryableStatusCode := range h.NonRetryableStatusCodes {
			if resp.StatusCode == nonRetryableStatusCode {
				h.Log.Errorf("Received non-retryable status %v. Metrics are lost.", resp.StatusCode)
				return nil
			}
		}

		errorLine := ""
		scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxErrMsgLen))
		if scanner.Scan() {
			errorLine = scanner.Text()
		}

		return fmt.Errorf("when writing to [%s] received status code: %d. body: %s", h.URL, resp.StatusCode, errorLine)
	}

	_, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("when writing to [%s] received error: %v", h.URL, err)
	}

	return nil
}

func init() {
	outputs.Add("http", func() telegraf.Output {
		return &HTTP{
			Method:         defaultMethod,
			URL:            defaultURL,
			UseBatchFormat: defaultUseBatchFormat,
		}
	})
}

func (h *HTTP) getAccessToken(ctx context.Context, audience string) (*oauth2.Token, error) {
	if h.oauth2Token.Valid() {
		return h.oauth2Token, nil
	}

	ts, err := idtoken.NewTokenSource(ctx, audience, idtoken.WithCredentialsFile(h.CredentialsFile))
	if err != nil {
		return nil, fmt.Errorf("error creating oauth2 token source: %s", err)
	}

	token, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("error fetching oauth2 token: %s", err)
	}

	h.oauth2Token = token

	return token, nil
}

func (h *HTTP) createHeaders(m telegraf.Metric) Headers {
	var headers = Headers{}

	for hdr, val := range h.Headers {
		headers[hdr] = val
	}

	for tag, hdr := range h.TagHeaders {
		val, ok := m.GetTag(tag)
		if ok {
			headers[hdr] = val
		}
	}

	return headers
}

func HashIDFromTagHeaders(m telegraf.Metric, tagheaders map[string]string) uint64 {
	h := fnv.New64a()
	for tag, _ := range tagheaders {
		val, ok := m.GetTag(tag)
		if ok {
			h.Write([]byte(tag))
			h.Write([]byte("\n"))
			h.Write([]byte(val))
			h.Write([]byte("\n"))
		}
	}
	return h.Sum64()
}

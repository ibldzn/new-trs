package fincloud

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

func newHTTPClient(caFile string, insecureTLS bool, timeout time.Duration) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecureTLS} //nolint:gosec // explicit emergency setting; false by default
	if caFile != "" {
		certificate, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read Fincloud CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system CA pool: %w", err)
		}
		if !pool.AppendCertsFromPEM(certificate) {
			return nil, fmt.Errorf("FINCLOUD_CA_FILE contains no valid certificates")
		}
		tlsConfig.RootCAs = pool
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsConfig,
			MaxIdleConns: 50, MaxIdleConnsPerHost: 20, MaxConnsPerHost: 32, IdleConnTimeout: 90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout,
		},
	}, nil
}

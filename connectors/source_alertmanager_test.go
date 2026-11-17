package connectors_test

import "time"

func init() {
	registerVendor("alertmanager", vendorDouble{
		// Alertmanager has no signature of its own: the operator points its
		// webhook receiver at an `http_config.authorization` credential, and
		// the receiver presents it as an ordinary bearer token
		// (https://prometheus.io/docs/alerting/latest/configuration/#http_config).
		// Nothing else authenticates the delivery, and nothing in the body
		// carries an instant, so this double signs no body and adds no
		// header beyond the credential.
		sign: func(secret string, _ time.Time, _ []byte) map[string]string {
			return map[string]string{"Authorization": "Bearer " + secret}
		},
	})
}

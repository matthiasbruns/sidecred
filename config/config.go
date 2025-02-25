package config

import (
	"encoding/json"
	"fmt"

	"sigs.k8s.io/yaml"

	"github.com/telia-oss/sidecred"
	"github.com/telia-oss/sidecred/provider/artifactory"
	"github.com/telia-oss/sidecred/provider/github"
	"github.com/telia-oss/sidecred/provider/random"
	"github.com/telia-oss/sidecred/provider/sts"
)

// Parse a YAML (or JSON) representation of sidecred.Config.
func Parse(b []byte) (cfg sidecred.Config, err error) {
	var t struct {
		Version *int `json:"version"`
	}
	err = yaml.Unmarshal(b, &t)
	if err != nil {
		return nil, fmt.Errorf("unmarshal version: %s", err)
	}
	if t.Version == nil {
		return nil, fmt.Errorf("%q must be defined", "version")
	}
	switch *t.Version {
	case 1:
		var v1 *v1
		err = yaml.UnmarshalStrict(b, &v1)
		cfg = v1
	default:
		return nil, fmt.Errorf("unknown configuration version (v%d)", *t.Version)
	}
	if err != nil {
		return nil, fmt.Errorf("unmarshal config (v%d): %s", *t.Version, err)
	}
	return cfg, nil
}

var _ sidecred.Config = &v1{}

type v1 struct {
	Version             int                     `json:"version"`
	CredentialNamespace string                  `json:"namespace"`
	CredentialStores    []*sidecred.StoreConfig `json:"stores"`
	CredentialRequests  []*requestV1            `json:"requests"`
}

// Namespace implements sidecred.Config.
func (c *v1) Namespace() string {
	return c.CredentialNamespace
}

// Stores implements sidecred.Config.
func (c *v1) Stores() []*sidecred.StoreConfig {
	return c.CredentialStores
}

// Requests implements sidecred.Config.
func (c *v1) Requests() (out []*sidecred.CredentialsMap) {
	for _, r := range c.CredentialRequests {
		out = append(out, r.credentialsMap())
	}
	return out
}

// Validate implements sidecred.Config.
func (c *v1) Validate() error {
	if c.CredentialNamespace == "" {
		return fmt.Errorf("%q must be defined", "namespace")
	}
	if len(c.CredentialStores) == 0 {
		return fmt.Errorf("%q must be defined", "stores")
	}

	stores := make(map[string]struct{}, len(c.CredentialStores))
	for i, s := range c.CredentialStores {
		switch s.Type {
		case sidecred.Inprocess, sidecred.SSM, sidecred.SecretsManager, sidecred.GithubSecrets:
		default:
			return fmt.Errorf("stores[%d]: unknown type %q", i, string(s.Type))
		}
		if _, found := stores[s.Alias()]; found {
			return fmt.Errorf("stores[%d]: duplicate store %q", i, s.Alias())
		}
		stores[s.Alias()] = struct{}{}
	}

	type requestsKey struct{ store, name string }
	requests := make(map[requestsKey]struct{}, len(c.CredentialRequests))

	for i, request := range c.CredentialRequests {
		if _, found := stores[request.Store]; !found {
			return fmt.Errorf("requests[%d]: undefined store %q", i, request.Store)
		}
		for ii, cred := range request.Creds {
			if err := cred.validate(); err != nil {
				return fmt.Errorf("requests[%d]: creds[%d]: %s", i, ii, err)
			}
			for _, r := range cred.flatten() {
				c, err := parseProviderConfig(r.Type, r.Config)
				if err != nil {
					return fmt.Errorf("requests[%d]: creds[%d]: parse config: %s", i, ii, err)
				}
				if err := c.Validate(); err != nil {
					return fmt.Errorf("requests[%d]: creds[%d]: invalid config: %s", i, ii, err)
				}
				key := requestsKey{store: request.Store, name: r.Name}
				if _, found := requests[key]; found {
					return fmt.Errorf("requests[%d]: creds[%d]: duplicated request %+v", i, ii, key)
				}
				requests[key] = struct{}{}
			}
		}
	}
	return nil
}

type requestV1 struct {
	Store string               `json:"store"`
	Creds []*credentialRequest `json:"creds"`
}

func (c *requestV1) credentialsMap() *sidecred.CredentialsMap {
	r := &sidecred.CredentialsMap{
		Store: c.Store,
	}
	for _, cred := range c.Creds {
		r.Credentials = append(r.Credentials, cred.flatten()...)
	}
	return r
}

// credentialRequest extends sidecred.CredentialRequest by allowing it to be defined in two ways:
// 1. As a regular CredentialRequest.
// 2. As a list of requests that share a CredentialType (nested credential requests should omit "type"):
//
//  - type: aws:sts
//    list:
// 	    - name: credential1
//        config ...
// 	    - name: credential2
//        config ...
//
type credentialRequest struct {
	*sidecred.CredentialRequest `json:",inline"`
	List                        []*sidecred.CredentialRequest `json:"list,omitempty"`
}

// validate the configRequest.
func (c *credentialRequest) validate() error {
	if len(c.List) == 0 {
		return nil // config.Validate covers the inlined request.
	}
	if c.CredentialRequest.Name != "" {
		return fmt.Errorf("%q should not be specified for lists", "name")
	}
	if len(c.CredentialRequest.Config) > 0 {
		return fmt.Errorf("%q should not be specified for lists", "config")
	}
	for i, r := range c.List {
		if r.Type != "" {
			return fmt.Errorf("list entry[%d]: request should not include %q", i, "type")
		}
	}
	return nil
}

// flatten returns the flattened list of credential requests.
func (c *credentialRequest) flatten() []*sidecred.CredentialRequest {
	if len(c.List) == 0 {
		return []*sidecred.CredentialRequest{c.CredentialRequest}
	}
	var requests []*sidecred.CredentialRequest
	for _, r := range c.List {
		requests = append(requests, &sidecred.CredentialRequest{
			Type:           c.CredentialRequest.Type,
			Name:           r.Name,
			RotationWindow: r.RotationWindow,
			Config:         r.Config,
		})
	}
	return requests
}

// parseProviderConfig from JSON.
func parseProviderConfig(t sidecred.CredentialType, config json.RawMessage) (sidecred.Validatable, error) {
	var c sidecred.Validatable
	switch t {
	case sidecred.AWSSTS:
		c = &sts.RequestConfig{}
	case sidecred.GithubAccessToken:
		c = &github.AccessTokenRequestConfig{}
	case sidecred.GithubDeployKey:
		c = &github.DeployKeyRequestConfig{}
	case sidecred.ArtifactoryAccessToken:
		c = &artifactory.RequestConfig{}
	case sidecred.Randomized:
		c = &random.RequestConfig{}
	default:
		return nil, fmt.Errorf("unknown type %q", string(t))
	}
	if err := sidecred.UnmarshalConfig(config, c); err != nil {
		return nil, fmt.Errorf("unmarshal config: %s", err)
	}
	return c, nil
}

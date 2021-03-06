package pki

import (
	"encoding/base64"
	"fmt"
	"time"

	"github.com/hashicorp/vault/helper/certutil"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
)

func pathGenerateRoot(b *backend) *framework.Path {
	ret := &framework.Path{
		Pattern: "root/generate/" + framework.GenericNameRegex("exported"),

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.WriteOperation: b.pathCAGenerateRoot,
		},

		HelpSynopsis:    pathGenerateRootHelpSyn,
		HelpDescription: pathGenerateRootHelpDesc,
	}

	ret.Fields = addCACommonFields(map[string]*framework.FieldSchema{})
	ret.Fields = addCAKeyGenerationFields(ret.Fields)
	ret.Fields = addCAIssueFields(ret.Fields)

	return ret
}

func pathSignIntermediate(b *backend) *framework.Path {
	ret := &framework.Path{
		Pattern: "root/sign-intermediate",

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.WriteOperation: b.pathCASignIntermediate,
		},

		HelpSynopsis:    pathSignIntermediateHelpSyn,
		HelpDescription: pathSignIntermediateHelpDesc,
	}

	ret.Fields = addCACommonFields(map[string]*framework.FieldSchema{})
	ret.Fields = addCAIssueFields(ret.Fields)

	ret.Fields["csr"] = &framework.FieldSchema{
		Type:        framework.TypeString,
		Default:     "",
		Description: `PEM-format CSR to be signed.`,
	}

	ret.Fields["use_csr_values"] = &framework.FieldSchema{
		Type:    framework.TypeBool,
		Default: false,
		Description: `If true, then:
1) Subject information, including names and alternate
names, will be preserved from the CSR rather than
using values provided in the other parameters to
this path;
2) Any key usages requested in the CSR will be
added to the basic set of key usages used for CA
certs signed by this path; for instance,
the non-repudiation flag.`,
	}

	return ret
}

func (b *backend) pathCAGenerateRoot(
	req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	var err error

	exported, format, role, errorResp := b.getGenerationParams(data)
	if errorResp != nil {
		return errorResp, nil
	}

	maxPathLengthIface, ok := data.GetOk("max_path_length")
	if ok {
		maxPathLength := maxPathLengthIface.(int)
		role.MaxPathLength = &maxPathLength
	}

	parsedBundle, err := generateCert(b, role, nil, true, req, data)
	if err != nil {
		switch err.(type) {
		case certutil.UserError:
			return logical.ErrorResponse(err.Error()), nil
		case certutil.InternalError:
			return nil, err
		}
	}

	cb, err := parsedBundle.ToCertBundle()
	if err != nil {
		return nil, fmt.Errorf("error converting raw cert bundle to cert bundle: %s", err)
	}

	resp := b.Secret(SecretCertsType).Response(
		map[string]interface{}{
			"expiration":    int64(parsedBundle.Certificate.NotAfter.Unix()),
			"serial_number": cb.SerialNumber,
			"certificate":   cb.Certificate,
			"issuing_ca":    cb.IssuingCA,
		},
		map[string]interface{}{
			"serial_number": cb.SerialNumber,
		})

	switch format {
	case "pem":
		if exported {
			resp.Data["private_key"] = cb.PrivateKey
			resp.Data["private_key_type"] = cb.PrivateKeyType
		}
	case "der":
		resp.Data["certificate"] = base64.StdEncoding.EncodeToString(parsedBundle.CertificateBytes)
		resp.Data["issuing_ca"] = base64.StdEncoding.EncodeToString(parsedBundle.IssuingCABytes)
		if exported {
			resp.Data["private_key"] = base64.StdEncoding.EncodeToString(parsedBundle.PrivateKeyBytes)
			resp.Data["private_key_type"] = cb.PrivateKeyType
		}
	}

	resp.Secret.TTL = parsedBundle.Certificate.NotAfter.Sub(time.Now())

	// Store it as the CA bundle
	entry, err := logical.StorageEntryJSON("config/ca_bundle", cb)
	if err != nil {
		return nil, err
	}
	err = req.Storage.Put(entry)
	if err != nil {
		return nil, err
	}

	// Also store it as just the certificate identified by serial number, so it
	// can be revoked
	err = req.Storage.Put(&logical.StorageEntry{
		Key:   "certs/" + cb.SerialNumber,
		Value: parsedBundle.CertificateBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("Unable to store certificate locally")
	}

	// For ease of later use, also store just the certificate at a known
	// location
	entry.Key = "ca"
	entry.Value = parsedBundle.CertificateBytes
	err = req.Storage.Put(entry)
	if err != nil {
		return nil, err
	}

	// Build a fresh CRL
	err = buildCRL(b, req)
	if err != nil {
		return nil, err
	}

	if parsedBundle.Certificate.MaxPathLen == 0 {
		resp.AddWarning("Max path length of the generated certificate is zero. This certificate cannot be used to issue intermediate CA certificates.")
	}

	return resp, nil
}

func (b *backend) pathCASignIntermediate(
	req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	var err error

	format := getFormat(data)
	if format == "" {
		return logical.ErrorResponse(
			`The "format" path parameter must be "pem" or "der"`,
		), nil
	}

	role := &roleEntry{
		TTL:              data.Get("ttl").(string),
		AllowLocalhost:   true,
		AllowAnyName:     true,
		AllowIPSANs:      true,
		EnforceHostnames: false,
	}

	if cn := data.Get("common_name").(string); len(cn) == 0 {
		role.UseCSRCommonName = true
	}

	var caErr error
	signingBundle, caErr := fetchCAInfo(req)
	switch caErr.(type) {
	case certutil.UserError:
		return nil, certutil.UserError{Err: fmt.Sprintf(
			"could not fetch the CA certificate (was one set?): %s", caErr)}
	case certutil.InternalError:
		return nil, certutil.InternalError{Err: fmt.Sprintf(
			"error fetching CA certificate: %s", caErr)}
	}

	useCSRValues := data.Get("use_csr_values").(bool)

	maxPathLengthIface, ok := data.GetOk("max_path_length")
	if ok {
		maxPathLength := maxPathLengthIface.(int)
		role.MaxPathLength = &maxPathLength
	}

	parsedBundle, err := signCert(b, role, signingBundle, true, useCSRValues, req, data)
	if err != nil {
		switch err.(type) {
		case certutil.UserError:
			return logical.ErrorResponse(err.Error()), nil
		case certutil.InternalError:
			return nil, err
		}
	}

	cb, err := parsedBundle.ToCertBundle()
	if err != nil {
		return nil, fmt.Errorf("Error converting raw cert bundle to cert bundle: %s", err)
	}

	resp := b.Secret(SecretCertsType).Response(
		map[string]interface{}{
			"expiration":    int64(parsedBundle.Certificate.NotAfter.Unix()),
			"serial_number": cb.SerialNumber,
			"certificate":   cb.Certificate,
			"issuing_ca":    cb.IssuingCA,
		},
		map[string]interface{}{
			"serial_number": cb.SerialNumber,
		})

	if format == "der" {
		resp.Data["certificate"] = base64.StdEncoding.EncodeToString(parsedBundle.CertificateBytes)
		resp.Data["issuing_ca"] = base64.StdEncoding.EncodeToString(parsedBundle.IssuingCABytes)
	}

	resp.Secret.TTL = parsedBundle.Certificate.NotAfter.Sub(time.Now())

	err = req.Storage.Put(&logical.StorageEntry{
		Key:   "certs/" + cb.SerialNumber,
		Value: parsedBundle.CertificateBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("Unable to store certificate locally")
	}

	if parsedBundle.Certificate.MaxPathLen == 0 {
		resp.AddWarning("Max path length of the signed certificate is zero. This certificate cannot be used to issue intermediate CA certificates.")
	}

	return resp, nil
}

const pathGenerateRootHelpSyn = `
Generate a new CA certificate and private key used for signing.
`

const pathGenerateRootHelpDesc = `
See the API documentation for more information.
`

const pathSignIntermediateHelpSyn = `
Issue an intermediate CA certificate based on the provided CSR.
`

const pathSignIntermediateHelpDesc = `
See the API documentation for more information.
`

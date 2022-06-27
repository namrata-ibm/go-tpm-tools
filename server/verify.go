package server

import (
	"crypto"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"

	"github.com/google/go-tpm-tools/internal"
	pb "github.com/google/go-tpm-tools/proto/attest"
	tpmpb "github.com/google/go-tpm-tools/proto/tpm"
	"github.com/google/go-tpm/tpm2"
	"google.golang.org/protobuf/proto"
)

// We conditinally support SHA-1 for PCR hashes, but at the lowest priority.
var pcrHashAlgs = append(internal.SignatureHashAlgs, tpm2.AlgSHA1)

var oidExtensionSubjectAltName = []int{2, 5, 29, 17}

var cloudComputeInstanceIdentifierOID asn1.ObjectIdentifier = []int{1, 3, 6, 1, 4, 1, 11129, 2, 1, 21}

// VerifyOpts allows for customizing the functionality of VerifyAttestation.
type VerifyOpts struct {
	// The nonce used when calling client.Attest
	Nonce []byte
	// Trusted public keys that can be used to directly verify the key used for
	// attestation. This option should be used if you already know the AK, as
	// it provides the highest level of assurance.
	TrustedAKs []crypto.PublicKey
	// Allow using SHA-1 PCRs to verify attestations. This defaults to false
	// because SHA-1 is a weak hash algorithm with known collision attacks.
	// However, setting this to true may be necessary if the client only
	// supports the legacy event log format. This is the case on older Linux
	// distributions (such as Debian 10). Note that this will NOT allow
	// SHA-1 signatures to be used, just SHA-1 PCRs.
	AllowSHA1 bool
	// A collection of trusted root CAs that are used to sign AK certificates.
	// The TrustedAKs are used first, followed by TrustRootCerts and
	// IntermediateCerts.
	// Adding a specific TPM manufacturer's root and intermediate CAs means all
	// TPMs signed by that CA will be trusted.
	TrustedRootCerts  []*x509.Certificate
	IntermediateCerts []*x509.Certificate
}

// TODO: Change int64 fields to uint64 when compatible with ASN1 parsing.
type gceSecurityProperties struct {
	SecurityVersion int64 `asn1:"explicit,tag:0,optional"`
	IsProduction    bool  `asn1:"explicit,tag:1,optional"`
}

type gceInstanceInfo struct {
	Zone               string `asn1:"utf8"`
	ProjectNumber      int64
	ProjectID          string `asn1:"utf8"`
	InstanceID         int64
	InstanceName       string                `asn1:"utf8"`
	SecurityProperties gceSecurityProperties `asn1:"explicit,optional"`
}

// VerifyAttestation performs the following checks on an Attestation:
//    - the AK used to generate the attestation is trusted (based on VerifyOpts)
//    - the provided signature is generated by the trusted AK public key
//    - the signature signs the provided quote data
//    - the quote data starts with TPM_GENERATED_VALUE
//    - the quote data is a valid TPMS_QUOTE_INFO
//    - the quote data was taken over the provided PCRs
//    - the provided PCR values match the quote data internal digest
//    - the provided opts.Nonce matches that in the quote data
//    - the provided eventlog matches the provided PCR values
//
// After this, the eventlog is parsed and the corresponding MachineState is
// returned. This design prevents unverified MachineStates from being used.
func VerifyAttestation(attestation *pb.Attestation, opts VerifyOpts) (*pb.MachineState, error) {
	if err := validateOpts(opts); err != nil {
		return nil, fmt.Errorf("bad options: %w", err)
	}

	var akPubKey crypto.PublicKey
	machineState := &pb.MachineState{}
	if len(attestation.GetAkCert()) == 0 {
		// If the AK Cert is not in the attestation, use the AK Public Area.
		akPubArea, err := tpm2.DecodePublic(attestation.GetAkPub())
		if err != nil {
			return nil, fmt.Errorf("failed to decode AK public area: %w", err)
		}
		akPubKey, err = akPubArea.Key()
		if err != nil {
			return nil, fmt.Errorf("failed to get AK public key: %w", err)
		}
		if err := validateAKPub(akPubKey, opts); err != nil {
			return nil, fmt.Errorf("failed to validate AK public key: %w", err)
		}
	} else {
		// If AK Cert is presented, ignore the AK Public Area.
		akCert, err := x509.ParseCertificate(attestation.GetAkCert())
		if err != nil {
			return nil, fmt.Errorf("failed to parse AK certificate: %w", err)
		}
		// Use intermediate certs from the attestation if they exist.
		certs, err := parseCerts(attestation.IntermediateCerts)
		if err != nil {
			return nil, fmt.Errorf("attestation intermediates: %w", err)
		}
		opts.IntermediateCerts = append(opts.IntermediateCerts, certs...)

		machineState, err = validateAKCert(akCert, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to validate AK certificate: %w", err)
		}
		akPubKey = akCert.PublicKey.(crypto.PublicKey)
	}

	// Attempt to replay the log against our PCRs in order of hash preference
	var lastErr error
	for _, quote := range supportedQuotes(attestation.GetQuotes()) {
		// Verify the Quote
		if err := internal.VerifyQuote(quote, akPubKey, opts.Nonce); err != nil {
			lastErr = fmt.Errorf("failed to verify quote: %w", err)
			continue
		}

		// Parse event logs and replay the events against the provided PCRs
		pcrs := quote.GetPcrs()
		state, err := parsePCClientEventLog(attestation.GetEventLog(), pcrs)
		if err != nil {
			lastErr = fmt.Errorf("failed to validate the PCClient event log: %w", err)
			continue
		}

		celState, err := parseCanonicalEventLog(attestation.GetCanonicalEventLog(), pcrs)
		if err != nil {
			lastErr = fmt.Errorf("failed to validate the Canonical event log: %w", err)
			continue
		}

		// Verify the PCR hash algorithm. We have this check here (instead of at
		// the start of the loop) so that the user gets a "SHA-1 not supported"
		// error only if allowing SHA-1 support would actually allow the log
		// to be verified. This makes debugging failed verifications easier.
		if !opts.AllowSHA1 && tpm2.Algorithm(pcrs.GetHash()) == tpm2.AlgSHA1 {
			lastErr = fmt.Errorf("SHA-1 is not allowed for verification (set VerifyOpts.AllowSHA1 to true to allow)")
			continue
		}

		proto.Merge(machineState, celState)
		proto.Merge(machineState, state)

		return machineState, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("attestation does not contain a supported quote")
}

func getInstanceInfo(extensions []pkix.Extension) (*pb.GCEInstanceInfo, error) {
	var rawInfo []byte
	for _, ext := range extensions {
		if ext.Id.Equal(cloudComputeInstanceIdentifierOID) {
			rawInfo = ext.Value
			break
		}
	}

	// If GCE Instance Info extension is not found.
	if len(rawInfo) == 0 {
		return nil, nil
	}

	info := gceInstanceInfo{}
	if _, err := asn1.Unmarshal(rawInfo, &info); err != nil {
		return nil, fmt.Errorf("failed to parse GCE Instance Information Extension: %w", err)
	}

	// TODO: Remove when fields are changed to uint64.
	if info.ProjectNumber < 0 || info.InstanceID < 0 || info.SecurityProperties.SecurityVersion < 0 {
		return nil, fmt.Errorf("negative integer fields found in GCE Instance Information Extension")
	}

	// Check production.
	if !info.SecurityProperties.IsProduction {
		return nil, nil
	}

	return &pb.GCEInstanceInfo{
		Zone:          info.Zone,
		ProjectId:     info.ProjectID,
		ProjectNumber: uint64(info.ProjectNumber),
		InstanceName:  info.InstanceName,
		InstanceId:    uint64(info.InstanceID),
	}, nil
}

// Check that we are passing in a valid VerifyOpts structure
func validateOpts(opts VerifyOpts) error {
	checkPub := len(opts.TrustedAKs) > 0
	checkCert := len(opts.TrustedRootCerts) > 0
	if !checkPub && !checkCert {
		return fmt.Errorf("no trust mechanism provided, either use TrustedAKs or TrustedRootCerts")
	}
	if checkPub && checkCert {
		return fmt.Errorf("multiple trust mechanisms provided, only use one of TrustedAKs or TrustedRootCerts")
	}
	return nil
}

func validateAKPub(ak crypto.PublicKey, opts VerifyOpts) error {
	for _, trusted := range opts.TrustedAKs {
		if internal.PubKeysEqual(ak, trusted) {
			return nil
		}
	}
	return fmt.Errorf("key not trusted")
}

func validateAKCert(akCert *x509.Certificate, opts VerifyOpts) (*pb.MachineState, error) {
	if len(opts.TrustedRootCerts) == 0 {
		return nil, validateAKPub(akCert.PublicKey.(crypto.PublicKey), opts)
	}

	// We manually handle the SAN extension because x509 marks it unhandled if
	// SAN does not parse any of DNSNames, EmailAddresses, IPAddresses, or URIs.
	// https://cs.opensource.google/go/go/+/master:src/crypto/x509/parser.go;l=668-678
	var exts []asn1.ObjectIdentifier
	for _, ext := range akCert.UnhandledCriticalExtensions {
		if ext.Equal(oidExtensionSubjectAltName) {
			continue
		}
		exts = append(exts, ext)
	}
	akCert.UnhandledCriticalExtensions = exts

	x509Opts := x509.VerifyOptions{
		Roots:         makePool(opts.TrustedRootCerts),
		Intermediates: makePool(opts.IntermediateCerts),
		// The default key usage (ExtKeyUsageServerAuth) is not appropriate for
		// an Attestation Key: ExtKeyUsage of
		// - https://oidref.com/2.23.133.8.1
		// - https://oidref.com/2.23.133.8.3
		// https://pkg.go.dev/crypto/x509#VerifyOptions
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsage(x509.ExtKeyUsageAny)},
	}
	if _, err := akCert.Verify(x509Opts); err != nil {
		return nil, fmt.Errorf("certificate did not chain to a trusted root: %v", err)
	}

	instanceInfo, err := getInstanceInfo(akCert.Extensions)
	if err != nil {
		return nil, fmt.Errorf("error getting instance info: %v", err)
	}

	return &pb.MachineState{Platform: &pb.PlatformState{InstanceInfo: instanceInfo}}, nil
}

// Retrieve the supported quotes in order of hash preference.
func supportedQuotes(quotes []*tpmpb.Quote) []*tpmpb.Quote {
	out := make([]*tpmpb.Quote, 0, len(quotes))
	for _, alg := range pcrHashAlgs {
		for _, quote := range quotes {
			if tpm2.Algorithm(quote.GetPcrs().GetHash()) == alg {
				out = append(out, quote)
				break
			}
		}
	}
	return out
}

func makePool(certs []*x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, cert := range certs {
		pool.AddCert(cert)
	}
	return pool
}

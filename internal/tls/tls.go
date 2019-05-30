/*
© Copyright IBM Corporation 2019

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package tls

import (
	"bufio"
	"fmt"
	"io/ioutil"
	pwr "math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"encoding/pem"

	"github.com/ibm-messaging/mq-container/internal/copy"
	"github.com/ibm-messaging/mq-container/internal/keystore"
	pkcs "software.sslmate.com/src/go-pkcs12"
)

// IntegrationDefaultLabel is the default certificate label used by Cloud Integration Platform
const IntegrationDefaultLabel = "default"

// P12TrustStoreName is the name of the PKCS#12 truststore used by the webconsole
const P12TrustStoreName = "trust.p12"

// CMSKeyStoreName is the name of the CMS Keystore used by the queue manager
const CMSKeyStoreName = "key.kdb"

type KeyStoreData struct {
	Keystore          *keystore.KeyStore
	Password          string
	TrustedCerts      []*pem.Block
	KnownFingerPrints []string
	KeyLabels         []string
}

type P12KeyFiles struct {
	Keystores []string
	Password  string
}

func getCertFingerPrint(block *pem.Block) (string, error) {
	// Add to future truststore and known certs (if not already there)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("could not parse x509 certificate: %v", err)
	}
	sha1Sum := sha1.Sum(cert.Raw)
	sha1str := string(sha1Sum[:])

	return sha1str, nil
}

// Add to Keystores known certs (if not already there) and add to the
// Keystore if "addToKeystore" is true.
func addCertToKeyData(block *pem.Block, keyData *KeyStoreData, addToKeystore bool) error {
	sha1str, err := getCertFingerPrint(block)
	if err != nil {
		return err
	}
	known := false
	for _, fingerprint := range keyData.KnownFingerPrints {
		if fingerprint == sha1str {
			known = true
			break
		}
	}

	if !known {
		// Sometimes we don't want to add to the keystore trust here.
		// For example if it will be imported with the key later.
		if addToKeystore {
			keyData.TrustedCerts = append(keyData.TrustedCerts, block)
		}
		keyData.KnownFingerPrints = append(keyData.KnownFingerPrints, sha1str)
	}
	return nil
}

// Generates a random 12 character password from the characters a-z, A-Z, 0-9.
func generateRandomPassword() string {
	pwr.Seed(time.Now().Unix())
	validChars := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	validcharArray := []byte(validChars)
	password := ""
	for i := 0; i < 12; i++ {
		password = password + string(validcharArray[pwr.Intn(len(validcharArray))])
	}

	return password
}

// Creates the PKCS#12 Truststore and the CMS Keystore.
func generateAllStores(dir string) (KeyStoreData, KeyStoreData, error) {
	var cmsKeystore, p12TrustStore KeyStoreData
	pw := generateRandomPassword()

	cmsKeystore.Password = pw
	p12TrustStore.Password = pw

	// Create the keystore Directory (if it doesn't already exist)
	os.MkdirAll(dir, 0775)

	p12TrustStore.Keystore = keystore.NewPKCS12KeyStore(filepath.Join(dir, P12TrustStoreName), p12TrustStore.Password)
	err := p12TrustStore.Keystore.Create()
	if err != nil {
		return cmsKeystore, p12TrustStore, fmt.Errorf("Failed to create PKCS#12 TrustStore: %v", err)
	}

	cmsKeystore.Keystore = keystore.NewCMSKeyStore(filepath.Join(dir, CMSKeyStoreName), cmsKeystore.Password)
	err = cmsKeystore.Keystore.Create()
	if err != nil {
		return cmsKeystore, p12TrustStore, fmt.Errorf("Failed to create CMS KeyStore: %v", err)
	}

	return cmsKeystore, p12TrustStore, nil
}

// processKeys walks through the keyDir directory and imports any keys it finds to individual PKCS#12 keystores
// and the CMS KeyStore. The label it uses is the name of the directory if finds the keys in.
func processKeys(keyDir, outputDir string, cmsKeyDB, p12TrustDB *KeyStoreData) (string, P12KeyFiles, error) {
	var p12s P12KeyFiles
	var firstLabel string

	pwToUse := cmsKeyDB.Password
	p12s.Password = pwToUse
	trustStoreReserveredName := P12TrustStoreName[0 : len(P12TrustStoreName)-len(filepath.Ext(P12TrustStoreName))]
	keyList, err := ioutil.ReadDir(keyDir)

	if err == nil && len(keyList) > 0 {
		// Found some keys, verify the contents
		for _, key := range keyList {
			keys, _ := ioutil.ReadDir(filepath.Join(keyDir, key.Name()))
			keyLabel := key.Name()
			if keyLabel == trustStoreReserveredName {
				return firstLabel, p12s, fmt.Errorf("Found key with same label set as same name as truststore(%s). This is not allowed", trustStoreReserveredName)
			}

			keyfilename := ""
			var keyfile interface{}
			var certFile *x509.Certificate
			var caFile []*x509.Certificate

			// find the keyfile name
			for _, a := range keys {
				if strings.HasSuffix(a.Name(), ".key") {
					keyFile, err := ioutil.ReadFile(filepath.Join(keyDir, key.Name(), a.Name()))
					if err != nil {
						return firstLabel, p12s, fmt.Errorf("Could not read keyfile %s: %v", filepath.Join(keyDir, key.Name(), a.Name()), err)
					}
					block, _ := pem.Decode(keyFile)
					if block == nil {
						return firstLabel, p12s, fmt.Errorf("Could not decode keyfile %s: pem.Decode returned nil", filepath.Join(keyDir, key.Name(), a.Name()))
					}

					//Test whether it is PKCS1
					keyfile, err = x509.ParsePKCS1PrivateKey(block.Bytes)
					if err != nil {
						// Before we fail check whether it is PKCS8
						keyfile, err = x509.ParsePKCS8PrivateKey(block.Bytes)
						if err != nil {
							fmt.Printf("key %s ParsePKCS1/8PrivateKey ERR: %v\n", filepath.Join(keyDir, key.Name(), a.Name()), err)
							return firstLabel, p12s, err
						}
						//It was PKCS8 afterall
					}
					keyfilename = a.Name()
				}
			}
			if keyfile == nil {
				continue
			}

			// Find out what the keyfile was called without the extension
			prefix := keyfilename[0 : len(keyfilename)-len(filepath.Ext(keyfilename))]

			for _, a := range keys {
				if strings.HasSuffix(a.Name(), ".key") {
					continue
				}
				if strings.HasPrefix(a.Name(), prefix) && strings.HasSuffix(a.Name(), ".crt") {
					cert, err := ioutil.ReadFile(filepath.Join(keyDir, key.Name(), a.Name()))
					if err != nil {
						return firstLabel, p12s, fmt.Errorf("Could not read file %s: %v", filepath.Join(keyDir, key.Name(), a.Name()), err)
					}
					block, _ := pem.Decode(cert)
					if block == nil {
						return firstLabel, p12s, fmt.Errorf("Could not decode certificate %s: pem.Decode returned nil", filepath.Join(keyDir, key.Name(), a.Name()))
					}
					certFile, err = x509.ParseCertificate(block.Bytes)
					if err != nil {
						return firstLabel, p12s, fmt.Errorf("Could not parse certificate %s: %v", filepath.Join(keyDir, key.Name(), a.Name()), err)
					}
					// Add to the dup list for the CMS keystore but not the PKCS#12 Truststore
					err = addCertToKeyData(block, cmsKeyDB, false)

				} else if strings.HasSuffix(a.Name(), ".crt") {
					remainder, err := ioutil.ReadFile(filepath.Join(keyDir, key.Name(), a.Name()))
					if err != nil {
						return firstLabel, p12s, fmt.Errorf("Could not read file %s: %v", filepath.Join(keyDir, key.Name(), a.Name()), err)
					}

					for string(remainder) != "" {
						var block *pem.Block
						block, remainder = pem.Decode(remainder)
						// If we can't decode the CA certificate then just exit.
						if block == nil {
							break
						}

						// Add to the dup list for the CMS keystore
						err = addCertToKeyData(block, cmsKeyDB, false)

						// Add to the p12 truststore
						err = addCertToKeyData(block, p12TrustDB, true)

						caCert, err := x509.ParseCertificate(block.Bytes)
						if err != nil {
							return firstLabel, p12s, fmt.Errorf("Could not parse CA certificate %s: %v", filepath.Join(keyDir, key.Name(), a.Name()), err)
						}

						caFile = append(caFile, caCert)
					}
				}
			}

			// Create p12 keystore
			file, err := pkcs.Encode(rand.Reader, keyfile, certFile, caFile, pwToUse)
			if err != nil {
				return firstLabel, p12s, fmt.Errorf("Could not encode PKCS#12 Keystore %s: %v", keyLabel+".p12", err)
			}

			err = ioutil.WriteFile(filepath.Join(outputDir, keyLabel+".p12"), file, 0644)
			if err != nil {
				return firstLabel, p12s, fmt.Errorf("Could not write PKCS#12 Keystore %s: %v", filepath.Join(outputDir, keyLabel+".p12"), err)
			}

			p12s.Keystores = append(p12s.Keystores, keyLabel+".p12")

			// Add to the CMS keystore
			err = cmsKeyDB.Keystore.Import(filepath.Join(outputDir, keyLabel+".p12"), pwToUse)
			if err != nil {
				return firstLabel, p12s, fmt.Errorf("Could not import keys from %s into CMS Keystore: %v", filepath.Join(outputDir, keyLabel+".p12"), err)
			}

			// Relabel it
			allLabels, err := cmsKeyDB.Keystore.GetCertificateLabels()
			if err != nil {
				return firstLabel, p12s, fmt.Errorf("Could not list keys in CMS Keystore: %v", err)
			}
			relabelled := false
			for _, cl := range allLabels {
				found := false
				for _, kl := range cmsKeyDB.KeyLabels {
					if strings.Trim(cl, "\"") == kl {
						found = true
						break
					}
				}
				if !found {
					// This is the one to rename
					err = cmsKeyDB.Keystore.RenameCertificate(strings.Trim(cl, "\""), keyLabel)
					if err != nil {
						return firstLabel, p12s, err
					}
					relabelled = true
					cmsKeyDB.KeyLabels = append(cmsKeyDB.KeyLabels, keyLabel)
					break
				}
			}

			if !relabelled {
				return firstLabel, p12s, fmt.Errorf("Unable to find the added key for %s in CMS keystore", keyLabel)
			}

			// First key found so mark it as the one to use with the queue manager.
			if firstLabel == "" {
				firstLabel = keyLabel
			}
		}
	}
	return firstLabel, p12s, nil
}

// processTrustCertificates walks through the trustDir directory and adds any certificates it finds
// to the PKCS#12 Truststore and the CMS KeyStore as long as has not already been added.
func processTrustCertificates(trustDir string, cmsKeyDB, p12TrustDB *KeyStoreData) error {
	certList, err := ioutil.ReadDir(trustDir)
	if err == nil && len(certList) > 0 {
		// Found some keys, verify the contents
		for _, cert := range certList {
			certs, _ := ioutil.ReadDir(filepath.Join(trustDir, cert.Name()))
			for _, a := range certs {
				if strings.HasSuffix(a.Name(), ".crt") {
					remainder, err := ioutil.ReadFile(filepath.Join(trustDir, cert.Name(), a.Name()))
					if err != nil {
						return fmt.Errorf("Could not read file %s: %v", filepath.Join(trustDir, cert.Name(), a.Name()), err)
					}

					for string(remainder) != "" {
						var block *pem.Block
						block, remainder = pem.Decode(remainder)
						if block == nil {
							break
						}

						// Add to the CMS keystore
						err = addCertToKeyData(block, cmsKeyDB, true)

						// Add to the p12 truststore
						err = addCertToKeyData(block, p12TrustDB, true)
					}
				}
			}
		}
	}
	// We've potentially created two lists of certificates to import. Add them both to relevant Truststores
	if len(p12TrustDB.TrustedCerts) > 0 {
		// Do P12 TrustStore first
		temporaryPemFile := filepath.Join("/tmp", "trust.pem")
		os.Remove(temporaryPemFile)

		err := writeCertsToFile(temporaryPemFile, p12TrustDB.TrustedCerts)
		if err != nil {
			return err
		}

		err = p12TrustDB.Keystore.AddNoLabel(temporaryPemFile)
		if err != nil {
			return fmt.Errorf("Could not add certificates to PKCS#12 Truststore: %v", err)
		}
	}

	if len(cmsKeyDB.TrustedCerts) > 0 {
		// Now the CMS Keystore
		temporaryPemFile := filepath.Join("/tmp", "cmsTrust.pem")
		os.Remove(temporaryPemFile)

		err = writeCertsToFile(temporaryPemFile, cmsKeyDB.TrustedCerts)
		if err != nil {
			return err
		}

		err = cmsKeyDB.Keystore.AddNoLabel(temporaryPemFile)
		if err != nil {
			return fmt.Errorf("Could not add certificates to CMS keystore: %v", err)
		}
	}
	return nil
}

// Writes a given list of certificates to a file.
func writeCertsToFile(file string, certs []*pem.Block) error {
	f, err := os.Create(file)
	if err != nil {
		return fmt.Errorf("writeCertsToFile: Could not create file %s: %v", file, err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)

	for i, c := range certs {
		err := pem.Encode(w, c)
		if err != nil {
			return fmt.Errorf("writeCertsToFile: Could not encode certificate number %d: %v", i, err)
		}
		w.Flush()
	}
	return nil
}

// ConfigureTLSKeystores sets up the  TLS Trust and Keystores for use
func ConfigureTLSKeystores(keyDir, certDir, outputDir string) (string, KeyStoreData, KeyStoreData, P12KeyFiles, error) {
	var returnLabel, label string
	var cmsKeyDB, p12TrustDB KeyStoreData
	var keyFiles P12KeyFiles
	var err error

	cmsKeyDB, p12TrustDB, err = generateAllStores(outputDir)
	if err != nil {
		return returnLabel, cmsKeyDB, p12TrustDB, keyFiles, err
	}

	err = handleIntegrationGeneratedCerts(keyDir)
	if err != nil {
		return returnLabel, cmsKeyDB, p12TrustDB, keyFiles, err
	}

	returnLabel, err = expandOldTLSVariable(keyDir, outputDir, &cmsKeyDB, &p12TrustDB)
	if err != nil {
		return returnLabel, cmsKeyDB, p12TrustDB, keyFiles, err
	}

	label, keyFiles, err = processKeys(keyDir, outputDir, &cmsKeyDB, &p12TrustDB)
	if err != nil {
		return returnLabel, cmsKeyDB, p12TrustDB, keyFiles, err
	}
	if returnLabel == "" {
		returnLabel = label
	}

	err = processTrustCertificates(certDir, &cmsKeyDB, &p12TrustDB)
	if err != nil {
		return returnLabel, cmsKeyDB, p12TrustDB, keyFiles, err
	}

	return returnLabel, cmsKeyDB, p12TrustDB, keyFiles, err
}

// This function supports an old mechanism of importing certificates
func handleIntegrationGeneratedCerts(keyDir string) error {
	dir := "/mnt/tls"
	outputdir := filepath.Join(keyDir, IntegrationDefaultLabel)
	keyfile := "tls.key"
	crtfile := "tls.crt"

	// check that the files exist, if not just quietly leave as there's nothing to import
	_, err := os.Stat(filepath.Join(dir, keyfile))
	if err != nil {
		return nil
	}

	_, err = os.Stat(filepath.Join(dir, crtfile))
	if err != nil {
		return nil
	}

	// Check the destination directory DOES not exist ahead of time
	_, err = os.Stat(outputdir)
	if err == nil {
		return fmt.Errorf("Found CIP certificates to import but a TLS secret called %s is already present", IntegrationDefaultLabel)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("Failed to check that %s does not exist: %v", outputdir, err)
	}

	err = os.MkdirAll(outputdir, 0775)
	if err != nil {
		return fmt.Errorf("Could not create %s: %v", outputdir, err)
	}

	err = copy.CopyFileMode(filepath.Join(dir, keyfile), filepath.Join(outputdir, keyfile), 0644)
	if err != nil {
		return fmt.Errorf("Could not copy %s: %v", keyfile, err)
	}

	err = copy.CopyFileMode(filepath.Join(dir, crtfile), filepath.Join(outputdir, crtfile), 0644)
	if err != nil {
		return fmt.Errorf("Could not copy %s: %v", keyfile, err)
	}

	// With certificates copied into place the rest of the TLS handling code will import them into the correct place
	return nil
}

// This function supports the old mechanism of importing certificates supplied by the MQ_TLS_KEYSTORE envvar
func expandOldTLSVariable(keyDir, outputDir string, cmsKeyDB, p12TrustDB *KeyStoreData) (string, error) {
	// TODO: Change this or find a way to set it
	outputDirName := "acopiedcertificate"

	// Check whether the old variable is set. If not exit quietly
	keyfile := os.Getenv("MQ_TLS_KEYSTORE")
	if keyfile == "" {
		return "", nil
	}

	// There is a file to read and process
	keyfilepw := os.Getenv("MQ_TLS_PASSPHRASE")

	if !strings.HasSuffix(keyfile, ".p12") {
		return "", fmt.Errorf("MQ_TLS_KEYSTORE (%s) does not point to a PKCS#12 file ending with the suffix .p12", keyfile)
	}

	_, err := os.Stat(keyfile)
	if err != nil {
		return "", fmt.Errorf("File %s referenced by MQ_TLS_KEYSTORE does not exist", keyfile)
	}

	readkey, err := ioutil.ReadFile(keyfile)
	if err != nil {
		return "", fmt.Errorf("Failed to read %s: %v", keyfile, err)
	}

	// File has been checked and read, decode it.
	pk, cert, cas, err := pkcs.DecodeChain(readkey, keyfilepw)
	if err != nil {
		return "", fmt.Errorf("Failed to decode %s: %v", keyfile, err)
	}

	// Find a directory name that doesn't exist
	for {
		_, err := os.Stat(filepath.Join(keyDir, outputDirName))
		if err == nil {
			outputDirName = outputDirName + "0"
		} else {
			break
		}
	}

	//Bceause they supplied this certificate using the old method we should use this for qm & webconsole
	overrideLabel := outputDirName

	// Write out the certificate for the private key
	if cert != nil {
		block := pem.Block{
			Type:    "CERTIFICATE",
			Headers: nil,
			Bytes:   cert.Raw,
		}
		err = addCertToKeyData(&block, cmsKeyDB, false)
		if err != nil {
			return "", fmt.Errorf("expandOldTLSVariable: Failed to add cert to CMS Keystore duplicate list: %v", err)
		}
		err = addCertToKeyData(&block, p12TrustDB, true)
		if err != nil {
			return "", fmt.Errorf("expandOldTLSVariable: Failed to add cert to P12 Truststore duplicate list: %v", err)
		}
	}

	// now write out all the ca certificates
	if cas != nil || len(cas) > 0 {
		for i, c := range cas {
			block := pem.Block{
				Type:    "CERTIFICATE",
				Headers: nil,
				Bytes:   c.Raw,
			}

			// Add to the dup list for the CMS keystore
			err = addCertToKeyData(&block, cmsKeyDB, false)
			if err != nil {
				return "", fmt.Errorf("expandOldTLSVariable: Failed to add CA cert %d to CMS Keystore duplicate list: %v", i, err)
			}

			// Add to the p12 truststore
			err = addCertToKeyData(&block, p12TrustDB, true)
			if err != nil {
				return "", fmt.Errorf("expandOldTLSVariable: Failed to add CA cert %d to P12 Truststore duplicate list: %v", i, err)
			}
		}
	}

	// Now we've handled the certificates copy the keystore into place
	destination := filepath.Join(outputDir, outputDirName+".p12")

	// Create p12 keystore
	file, err := pkcs.Encode(rand.Reader, pk, cert, cas, p12TrustDB.Password)
	if err != nil {
		return "", fmt.Errorf("Failed to re-encode p12 keystore: %v", err)
	}

	err = ioutil.WriteFile(destination, file, 0644)
	if err != nil {
		return "", fmt.Errorf("Failed to write p12 keystore: %v", err)
	}

	// Add to the CMS keystore
	err = cmsKeyDB.Keystore.Import(destination, p12TrustDB.Password)
	if err != nil {
		return "", fmt.Errorf("Failed to import p12 keystore %s: %v", destination, err)
	}

	if pk != nil {
		// Relabel the key
		allLabels, err := cmsKeyDB.Keystore.GetCertificateLabels()
		if err != nil {
			fmt.Printf("cms GetCertificateLabels: %v\n", err)
			return "", err
		}
		relabelled := false
		for _, cl := range allLabels {
			found := false
			for _, kl := range cmsKeyDB.KeyLabels {
				if strings.Trim(cl, "\"") == kl {
					found = true
					break
				}
			}
			if !found {
				// This is the one to rename
				err = cmsKeyDB.Keystore.RenameCertificate(strings.Trim(cl, "\""), outputDirName)
				if err != nil {
					return "", err
				}
				relabelled = true
				cmsKeyDB.KeyLabels = append(cmsKeyDB.KeyLabels, outputDirName)
				break
			}
		}

		if !relabelled {
			return "", fmt.Errorf("Unable to find the added key in CMS keystore")
		}
	}

	return overrideLabel, nil
}

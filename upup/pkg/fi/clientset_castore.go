/*
Copyright 2016 The Kubernetes Authors.

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

package fi

import (
	"bytes"
	crypto_rand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"github.com/golang/glog"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kops/pkg/apis/kops"
	kopsinternalversion "k8s.io/kops/pkg/client/clientset_generated/clientset/typed/kops/internalversion"
	"k8s.io/kops/pkg/pki"
	"k8s.io/kops/util/pkg/vfs"
	"math/big"
	"sync"
	"time"
)

type ClientsetCAStore struct {
	namespace string
	clientset kopsinternalversion.KopsInterface

	mutex         sync.Mutex
	cacheCaKeyset *keyset
}

var _ CAStore = &ClientsetCAStore{}

// NewClientsetCAStore is the constructor for ClientsetCAStore
func NewClientsetCAStore(clientset kopsinternalversion.KopsInterface, namespace string) CAStore {
	c := &ClientsetCAStore{
		clientset: clientset,
		namespace: namespace,
	}

	return c
}

// readCAKeypairs retrieves the CA keypair, generating a new keypair if not found
func (c *ClientsetCAStore) readCAKeypairs() (*keyset, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.cacheCaKeyset != nil {
		return c.cacheCaKeyset, nil
	}

	keyset, err := c.loadKeyset(CertificateId_CA)
	if err != nil {
		return nil, err
	}

	if keyset == nil {
		keyset, err = c.generateCACertificate()
		if err != nil {
			return nil, err
		}

	}
	c.cacheCaKeyset = keyset

	return keyset, nil
}

// generateCACertificate creates and stores a CA keypair
// Should be called with the mutex held, to prevent concurrent creation of different keys
func (c *ClientsetCAStore) generateCACertificate() (*keyset, error) {
	template := BuildCAX509Template()

	caRsaKey, err := rsa.GenerateKey(crypto_rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("error generating RSA private key: %v", err)
	}

	caPrivateKey := &pki.PrivateKey{Key: caRsaKey}

	caCertificate, err := pki.SignNewCertificate(caPrivateKey, template, nil, nil)
	if err != nil {
		return nil, err
	}

	t := time.Now().UnixNano()
	serial := pki.BuildPKISerial(t)
	id := serial.String()

	err = c.storeKeypair(CertificateId_CA, id, caCertificate, caPrivateKey)
	if err != nil {
		return nil, err
	}

	// Make double-sure it round-trips
	keyset, err := c.loadKeyset(CertificateId_CA)
	if err != nil {
		return nil, err
	}
	if keyset == nil || keyset.primary == nil || keyset.primary.id != id {
		return nil, fmt.Errorf("failed to round-trip CA keyset")
	}

	return keyset, nil
}

// keyset is a parsed Keyset
type keyset struct {
	items   map[string]*keysetItem
	primary *keysetItem
}

// keysetItem is a parsed KeysetItem
type keysetItem struct {
	id          string
	certificate *pki.Certificate
	privateKey  *pki.PrivateKey
}

// loadKeyset gets the named keyset
func (c *ClientsetCAStore) loadKeyset(name string) (*keyset, error) {
	o, err := c.clientset.Keysets(c.namespace).Get(name, v1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error reading keyset %q: %v", name, err)
	}

	keyset := &keyset{
		items: make(map[string]*keysetItem),
	}

	for _, key := range o.Spec.Keys {
		cert, err := pki.LoadPEMCertificate(key.PublicMaterial)
		if err != nil {
			glog.Warningf("key public material was %s", key.PublicMaterial)
			return nil, fmt.Errorf("error loading certificate %s/%s: %v", name, key.Id, err)
		}
		privateKey, err := pki.ParsePEMPrivateKey(key.PrivateMaterial)
		if err != nil {
			return nil, fmt.Errorf("error loading private key %s/%s: %v", name, key.Id, err)
		}
		keyset.items[key.Id] = &keysetItem{
			id:          key.Id,
			certificate: cert,
			privateKey:  privateKey,
		}
	}

	primary := FindPrimary(o)
	if primary != nil {
		keyset.primary = keyset.items[primary.Id]
	}

	return keyset, nil
}

// FindPrimary returns the primary KeysetItem in the Keyset
func FindPrimary(keyset *kops.Keyset) *kops.KeysetItem {
	var primary *kops.KeysetItem
	var primaryVersion *big.Int
	for i := range keyset.Spec.Keys {
		item := &keyset.Spec.Keys[i]
		version, ok := big.NewInt(0).SetString(item.Id, 10)
		if !ok {
			glog.Warningf("Ignoring key item with non-integer version: %q", item.Id)
			continue
		}

		if primaryVersion == nil || version.Cmp(primaryVersion) > 0 {
			primary = item
			primaryVersion = version
		}
	}
	return primary
}

// Cert implements CAStore::Cert
func (c *ClientsetCAStore) Cert(name string, createIfMissing bool) (*pki.Certificate, error) {
	cert, err := c.FindCert(name)
	if err == nil && cert == nil {
		if !createIfMissing {
			glog.Warningf("using empty certificate, because running with DryRun")
			return &pki.Certificate{}, err
		}
		return nil, fmt.Errorf("cannot find certificate %q", name)
	}
	return cert, err

}

// CertificatePool implements CAStore::CertificatePool
func (c *ClientsetCAStore) CertificatePool(id string, createIfMissing bool) (*CertificatePool, error) {
	cert, err := c.FindCertificatePool(id)
	if err == nil && cert == nil {
		if !createIfMissing {
			glog.Warningf("using empty certificate, because running with DryRun")
			return &CertificatePool{}, err
		}
		return nil, fmt.Errorf("cannot find certificate pool %q", id)
	}
	return cert, err

}

// FindKeypair implements CAStore::FindKeypair
func (c *ClientsetCAStore) FindKeypair(name string) (*pki.Certificate, *pki.PrivateKey, error) {
	keyset, err := c.loadKeyset(name)
	if err != nil {
		return nil, nil, err
	}

	if keyset != nil && keyset.primary != nil {
		return keyset.primary.certificate, keyset.primary.privateKey, nil
	}

	return nil, nil, nil
}

// FindCert implements CAStore::FindCert
func (c *ClientsetCAStore) FindCert(name string) (*pki.Certificate, error) {
	keyset, err := c.loadKeyset(name)
	if err != nil {
		return nil, err
	}

	var cert *pki.Certificate
	if keyset != nil && keyset.primary != nil {
		cert = keyset.primary.certificate
	}

	return cert, nil
}

// FindCertificatePool implements CAStore::FindCertificatePool
func (c *ClientsetCAStore) FindCertificatePool(name string) (*CertificatePool, error) {
	keyset, err := c.loadKeyset(name)
	if err != nil {
		return nil, err
	}

	pool := &CertificatePool{}

	if keyset != nil {
		if keyset.primary != nil {
			pool.Primary = keyset.primary.certificate
		}

		for id, item := range keyset.items {
			if id == keyset.primary.id {
				continue
			}
			pool.Secondary = append(pool.Secondary, item.certificate)
		}
	}
	return pool, nil
}

// List implements CAStore::List
func (c *ClientsetCAStore) List() ([]*KeystoreItem, error) {
	var items []*KeystoreItem

	{
		list, err := c.clientset.Keysets(c.namespace).List(v1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("error listing Keysets: %v", err)
		}

		for _, keyset := range list.Items {
			for _, item := range keyset.Spec.Keys {
				ki := &KeystoreItem{
					Name: keyset.Name,
					Id:   item.Id,
				}

				switch keyset.Spec.Type {
				case kops.SecretTypeKeypair:
					ki.Type = SecretTypeKeypair
				case kops.SecretTypeSecret:
					//ki.Type = SecretTypeSecret
					continue // Ignore - this is handled by ClientsetSecretStore
				default:
					return nil, fmt.Errorf("unhandled secret type %q: %v", ki.Type, err)
				}
				items = append(items, ki)
			}
		}
	}

	{
		list, err := c.clientset.SSHCredentials(c.namespace).List(v1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("error listing SSHCredentials: %v", err)
		}

		for _, sshCredential := range list.Items {
			ki := &KeystoreItem{
				Name: sshCredential.Name,
				Type: SecretTypeSSHPublicKey,
			}
			items = append(items, ki)
		}
	}

	return items, nil
}

// IssueCert implements CAStore::IssueCert
func (c *ClientsetCAStore) IssueCert(name string, serial *big.Int, privateKey *pki.PrivateKey, template *x509.Certificate) (*pki.Certificate, error) {
	glog.Infof("Issuing new certificate: %q", name)

	template.SerialNumber = serial

	caKeyset, err := c.readCAKeypairs()
	if err != nil {
		return nil, err
	}

	if caKeyset == nil {
		return nil, fmt.Errorf("ca keyset was not found; cannot issue certificates")
	}
	if caKeyset.primary == nil {
		return nil, fmt.Errorf("ca keyset did not have any key data; cannot issue certificates")
	}
	if caKeyset.primary.certificate == nil {
		return nil, fmt.Errorf("ca certificate was not found; cannot issue certificates")
	}
	if caKeyset.primary.privateKey == nil {
		return nil, fmt.Errorf("ca privateKey was not found; cannot issue certificates")
	}
	cert, err := pki.SignNewCertificate(privateKey, template, caKeyset.primary.certificate.Certificate, caKeyset.primary.privateKey)
	if err != nil {
		return nil, err
	}

	err = c.StoreKeypair(name, cert, privateKey)
	if err != nil {
		return nil, err
	}

	// Make double-sure it round-trips
	keyset, err := c.loadKeyset(name)
	if err != nil {
		return nil, fmt.Errorf("error fetching issued certificate: %v", err)
	}

	if keyset == nil {
		return nil, fmt.Errorf("issued certificate not found: %v", err)
	}
	if keyset.primary == nil {
		return nil, fmt.Errorf("issued certificate did not have data: %v", err)
	}
	if keyset.primary.id != serial.String() {
		return nil, fmt.Errorf("issued certificate changed concurrently (id mismatch)")
	}
	return keyset.primary.certificate, nil
}

// StoreKeypair implements CAStore::StoreKeypair
func (c *ClientsetCAStore) StoreKeypair(name string, cert *pki.Certificate, privateKey *pki.PrivateKey) error {
	serial := cert.Certificate.SerialNumber

	return c.storeKeypair(name, serial.String(), cert, privateKey)
}

// AddCert implements CAStore::AddCert
func (c *ClientsetCAStore) AddCert(name string, cert *pki.Certificate) error {
	glog.Infof("Adding TLS certificate: %q", name)

	// We add with a timestamp of zero so this will never be the newest cert
	serial := pki.BuildPKISerial(0)

	err := c.storeKeypair(name, serial.String(), cert, nil)
	if err != nil {
		return err
	}

	return nil
}

// FindPrivateKey implements CAStore::FindPrivateKey
func (c *ClientsetCAStore) FindPrivateKey(name string) (*pki.PrivateKey, error) {
	keyset, err := c.loadKeyset(name)
	if err != nil {
		return nil, err
	}

	if keyset != nil && keyset.primary != nil {
		return keyset.primary.privateKey, nil
	}
	return nil, nil
}

// PrivateKey implements CAStore::PrivateKey
func (c *ClientsetCAStore) PrivateKey(name string, createIfMissing bool) (*pki.PrivateKey, error) {
	key, err := c.FindPrivateKey(name)
	if err == nil && key == nil {
		if !createIfMissing {
			glog.Warningf("using empty certificate, because running with DryRun")
			return &pki.PrivateKey{}, err
		}
		return nil, fmt.Errorf("cannot find SSL key %q", name)
	}
	return key, err
}

// CreateKeypair implements CAStore::CreateKeypair
func (c *ClientsetCAStore) CreateKeypair(id string, template *x509.Certificate, privateKey *pki.PrivateKey) (*pki.Certificate, error) {
	serial := c.buildSerial()

	cert, err := c.IssueCert(id, serial, privateKey, template)
	if err != nil {
		return nil, err
	}

	return cert, nil
}

// addKey saves the specified key to the registry
func (c *ClientsetCAStore) addKey(name string, keysetType kops.KeysetType, item *kops.KeysetItem) error {
	create := false
	client := c.clientset.Keysets(c.namespace)
	keyset, err := client.Get(name, v1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			keyset = nil
		} else {
			return fmt.Errorf("error reading keyset %q: %v", name, err)
		}
	}
	if keyset == nil {
		keyset = &kops.Keyset{}
		keyset.Name = name
		keyset.Spec.Type = keysetType
		create = true
	}
	keyset.Spec.Keys = append(keyset.Spec.Keys, *item)
	if create {
		if _, err := client.Create(keyset); err != nil {
			return fmt.Errorf("error creating keyset %q: %v", name, err)
		}
	} else {
		if _, err := client.Update(keyset); err != nil {
			return fmt.Errorf("error updating keyset %q: %v", name, err)
		}
	}
	return nil
}

// DeleteKeysetItem deletes the specified key from the registry; deleting the whole keyset if it was the last one
func DeleteKeysetItem(client kopsinternalversion.KeysetInterface, name string, keysetType kops.KeysetType, id string) error {
	keyset, err := client.Get(name, v1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		} else {
			return fmt.Errorf("error reading Keyset %q: %v", name, err)
		}
	}

	if keyset.Spec.Type != keysetType {
		return fmt.Errorf("mismatch on Keyset type on %q", name)
	}

	var newKeys []kops.KeysetItem
	found := false
	for _, ki := range keyset.Spec.Keys {
		if ki.Id == id {
			found = true
		} else {
			newKeys = append(newKeys, ki)
		}
	}
	if !found {
		return fmt.Errorf("KeysetItem %q not found in Keyset %q", id, name)
	}
	if len(newKeys) == 0 {
		if err := client.Delete(name, &v1.DeleteOptions{}); err != nil {
			return fmt.Errorf("error deleting Keyset %q: %v", name, err)
		}
	} else {
		keyset.Spec.Keys = newKeys
		if _, err := client.Update(keyset); err != nil {
			return fmt.Errorf("error updating Keyset %q: %v", name, err)
		}
	}
	return nil
}

// addSshCredential saves the specified SSH Credential to the registry, doing an update or insert
func (c *ClientsetCAStore) addSshCredential(name string, publicKey string) error {
	create := false
	client := c.clientset.SSHCredentials(c.namespace)
	sshCredential, err := client.Get(name, v1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			sshCredential = nil
		} else {
			return fmt.Errorf("error reading SSHCredential %q: %v", name, err)
		}
	}
	if sshCredential == nil {
		sshCredential = &kops.SSHCredential{}
		sshCredential.Name = name
		create = true
	}
	sshCredential.Spec.PublicKey = publicKey
	if create {
		if _, err := client.Create(sshCredential); err != nil {
			return fmt.Errorf("error creating SSHCredential %q: %v", name, err)
		}
	} else {
		if _, err := client.Update(sshCredential); err != nil {
			return fmt.Errorf("error updating SSHCredential %q: %v", name, err)
		}
	}
	return nil
}

// deleteSSHCredential deletes the specified SSHCredential from the registry
func (c *ClientsetCAStore) deleteSSHCredential(name string) error {
	client := c.clientset.SSHCredentials(c.namespace)
	err := client.Delete(name, &v1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("error deleting SSHCredential %q: %v", name, err)
	}
	return nil
}

// addKey saves the specified keypair to the registry
func (c *ClientsetCAStore) storeKeypair(name string, id string, cert *pki.Certificate, privateKey *pki.PrivateKey) error {
	var publicMaterial bytes.Buffer
	if _, err := cert.WriteTo(&publicMaterial); err != nil {
		return err
	}

	var privateMaterial bytes.Buffer
	if _, err := privateKey.WriteTo(&privateMaterial); err != nil {
		return err
	}

	item := &kops.KeysetItem{
		Id:              id,
		PublicMaterial:  publicMaterial.Bytes(),
		PrivateMaterial: privateMaterial.Bytes(),
	}
	return c.addKey(name, kops.SecretTypeKeypair, item)
}

// buildSerial returns a serial for use when issuing certificates
func (c *ClientsetCAStore) buildSerial() *big.Int {
	t := time.Now().UnixNano()
	return pki.BuildPKISerial(t)
}

// AddSSHPublicKey implements CAStore::AddSSHPublicKey
func (c *ClientsetCAStore) AddSSHPublicKey(name string, pubkey []byte) error {
	_, _, _, _, err := ssh.ParseAuthorizedKey(pubkey)
	if err != nil {
		return fmt.Errorf("error parsing SSH public key: %v", err)
	}

	// TODO: Reintroduce or remove
	//// compute fingerprint to serve as id
	//h := md5.New()
	//_, err = h.Write(sshPublicKey.Marshal())
	//if err != nil {
	//	return err
	//}
	//id = formatFingerprint(h.Sum(nil))

	return c.addSshCredential(name, string(pubkey))
}

// FindSSHPublicKeys implements CAStore::FindSSHPublicKeys
func (c *ClientsetCAStore) FindSSHPublicKeys(name string) ([]*KeystoreItem, error) {
	o, err := c.clientset.SSHCredentials(c.namespace).Get(name, v1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error reading SSHCredential %q: %v", name, err)
	}

	var items []*KeystoreItem
	item := &KeystoreItem{
		Type: SecretTypeSSHPublicKey,
		Name: name,
		//Id:   insertFingerprintColons(k.Id),
		Data: []byte(o.Spec.PublicKey),
	}
	items = append(items, item)

	return items, nil
}

// DeleteSecret implements CAStore::DeleteSecret
func (c *ClientsetCAStore) DeleteSecret(item *KeystoreItem) error {
	switch item.Type {
	case SecretTypeSSHPublicKey:
		return c.deleteSSHCredential(item.Name)

	case SecretTypeKeypair:
		client := c.clientset.Keysets(c.namespace)
		return DeleteKeysetItem(client, item.Name, kops.SecretTypeKeypair, item.Id)

	default:
		// Primarily because we need to make sure users can recreate them!
		return fmt.Errorf("deletion of keystore items of type %v not (yet) supported", item.Type)
	}
}

// VFSPath implements CAStore::VFSPath
func (c *ClientsetCAStore) VFSPath() vfs.Path {
	// We will implement mirroring instead
	panic("ClientsetCAStore::VFSPath not implemented")
}

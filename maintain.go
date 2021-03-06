// Copyright 2015 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package certmagic

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"path"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/ocsp"
)

// maintainAssets is a permanently-blocking function
// that loops indefinitely and, on a regular schedule, checks
// certificates for expiration and initiates a renewal of certs
// that are expiring soon. It also updates OCSP stapling. It
// should only be called once per cache. Panics are recovered.
func (certCache *Cache) maintainAssets() {
	defer func() {
		if err := recover(); err != nil {
			buf := make([]byte, stackTraceBufferSize)
			buf = buf[:runtime.Stack(buf, false)]
			log.Printf("panic: certificate maintenance: %v\n%s", err, buf)
		}
	}()

	renewalTicker := time.NewTicker(certCache.options.RenewCheckInterval)
	ocspTicker := time.NewTicker(certCache.options.OCSPCheckInterval)

	log.Printf("[INFO][cache:%p] Started certificate maintenance routine", certCache)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for {
		select {
		case <-renewalTicker.C:
			err := certCache.RenewManagedCertificates(ctx)
			if err != nil {
				log.Printf("[ERROR][cache:%p] Renewing managed certificates: %v", certCache, err)
			}
		case <-ocspTicker.C:
			certCache.updateOCSPStaples(ctx)
		case <-certCache.stopChan:
			renewalTicker.Stop()
			ocspTicker.Stop()
			// TODO: stop any in-progress maintenance operations and clear locks we made (this might be done now with our use of context)
			log.Printf("[INFO][cache:%p] Stopped certificate maintenance routine", certCache)
			close(certCache.doneChan)
			return
		}
	}
}

// RenewManagedCertificates renews managed certificates,
// including ones loaded on-demand. Note that this is done
// automatically on a regular basis; normally you will not
// need to call this. This method assumes non-interactive
// mode (i.e. operating in the background).
func (certCache *Cache) RenewManagedCertificates(ctx context.Context) error {
	// configs will hold a map of certificate name to the config
	// to use when managing that certificate
	configs := make(map[string]*Config)

	// we use the queues for a very important reason: to do any and all
	// operations that could require an exclusive write lock outside
	// of the read lock! otherwise we get a deadlock, yikes. in other
	// words, our first iteration through the certificate cache does NOT
	// perform any operations--only queues them--so that more fine-grained
	// write locks may be obtained during the actual operations.
	var renewQueue, reloadQueue, deleteQueue []Certificate

	certCache.mu.RLock()
	for certKey, cert := range certCache.cache {
		if !cert.managed {
			continue
		}

		// the list of names on this cert should never be empty... programmer error?
		if cert.Names == nil || len(cert.Names) == 0 {
			log.Printf("[WARNING] Certificate keyed by '%s' has no names: %v - removing from cache", certKey, cert.Names)
			deleteQueue = append(deleteQueue, cert)
			continue
		}

		// get the config associated with this certificate
		cfg, err := certCache.getConfig(cert)
		if err != nil {
			log.Printf("[ERROR] Getting configuration to manage certificate for names %v; unable to renew: %v", cert.Names, err)
			continue
		}
		if cfg == nil {
			// this is bad if this happens, probably a programmer error (oops)
			log.Printf("[ERROR] No configuration associated with certificate for names %v; unable to manage", cert.Names)
			continue
		}

		// if time is up or expires soon, we need to try to renew it
		if cert.NeedsRenewal(cfg) {
			configs[cert.Names[0]] = cfg

			// see if the certificate in storage has already been renewed, possibly by another
			// instance that didn't coordinate with this one; if so, just load it (this
			// might happen if another instance already renewed it - kinda sloppy but checking disk
			// first is a simple way to possibly drastically reduce rate limit problems)
			storedCertExpiring, err := cfg.managedCertInStorageExpiresSoon(cert)
			if err != nil {
				// hmm, weird, but not a big deal, maybe it was deleted or something
				log.Printf("[NOTICE] Error while checking if certificate for %v in storage is also expiring soon: %v",
					cert.Names, err)
			} else if !storedCertExpiring {
				// if the certificate is NOT expiring soon and there was no error, then we
				// are good to just reload the certificate from storage instead of repeating
				// a likely-unnecessary renewal procedure
				reloadQueue = append(reloadQueue, cert)
				continue
			}

			// the certificate in storage has not been renewed yet, so we will do it
			// NOTE: It is super-important to note that the TLS-ALPN challenge requires
			// a write lock on the cache in order to complete its challenge, so it is extra
			// vital that this renew operation does not happen inside our read lock!
			renewQueue = append(renewQueue, cert)
		}
	}
	certCache.mu.RUnlock()

	// Reload certificates that merely need to be updated in memory
	for _, oldCert := range reloadQueue {
		timeLeft := oldCert.Leaf.NotAfter.Sub(time.Now().UTC())
		log.Printf("[INFO] %v Maintenance routine: certificate expires in %s, but is already renewed in storage; reloading stored certificate",
			oldCert.Names, timeLeft)

		cfg := configs[oldCert.Names[0]]

		// crucially, this happens OUTSIDE a lock on the certCache
		err := cfg.reloadManagedCertificate(oldCert)
		if err != nil {
			log.Printf("[ERROR] Loading renewed certificate: %v", err)
			continue
		}
	}

	// Renewal queue
	for _, oldCert := range renewQueue {
		cfg := configs[oldCert.Names[0]]
		err := certCache.queueRenewalTask(ctx, oldCert, cfg)
		if err != nil {
			log.Printf("[ERROR] %v", err)
			continue
		}
	}

	// Deletion queue
	certCache.mu.Lock()
	for _, cert := range deleteQueue {
		certCache.removeCertificate(cert)
	}
	certCache.mu.Unlock()

	return nil
}

func (certCache *Cache) queueRenewalTask(ctx context.Context, oldCert Certificate, cfg *Config) error {
	timeLeft := oldCert.Leaf.NotAfter.Sub(time.Now().UTC())
	log.Printf("[INFO] %v Maintenance routine: certificate expires in %v; queueing for renewal", oldCert.Names, timeLeft)

	// Get the name which we should use to renew this certificate;
	// we only support managing certificates with one name per cert,
	// so this should be easy.
	renewName := oldCert.Names[0]

	// queue up this renewal job (is a no-op if already active or queued)
	jm.Submit("renew_"+renewName, func() error {
		timeLeft := oldCert.Leaf.NotAfter.Sub(time.Now().UTC())
		log.Printf("[INFO] %v Maintenance routine: attempting renewal with %v remaining", oldCert.Names, timeLeft)

		// perform renewal - crucially, this happens OUTSIDE a lock on certCache
		err := cfg.RenewCert(ctx, renewName, false)
		if err != nil {
			if cfg.OnDemand != nil {
				// loaded dynamically, remove dynamically
				certCache.mu.Lock()
				certCache.removeCertificate(oldCert)
				certCache.mu.Unlock()
			}
			return fmt.Errorf("%v %v", oldCert.Names, err)
		}

		// successful renewal, so update in-memory cache by loading
		// renewed certificate so it will be used with handshakes
		err = cfg.reloadManagedCertificate(oldCert)
		if err != nil {
			return ErrNoRetry{fmt.Errorf("%v %v", oldCert.Names, err)}
		}
		return nil
	})

	return nil
}

// updateOCSPStaples updates the OCSP stapling in all
// eligible, cached certificates.
//
// OCSP maintenance strives to abide the relevant points on
// Ryan Sleevi's recommendations for good OCSP support:
// https://gist.github.com/sleevi/5efe9ef98961ecfb4da8
func (certCache *Cache) updateOCSPStaples(ctx context.Context) {
	// temporary structures to store updates or tasks
	// so that we can keep our locks short-lived
	type ocspUpdate struct {
		rawBytes []byte
		parsed   *ocsp.Response
	}
	type updateQueueEntry struct {
		cert           Certificate
		certHash       string
		lastNextUpdate time.Time
	}
	updated := make(map[string]ocspUpdate)
	var updateQueue []updateQueueEntry
	var renewQueue []Certificate
	configs := make(map[string]*Config)

	// obtain brief read lock during our scan to see which staples need updating
	certCache.mu.RLock()
	for certHash, cert := range certCache.cache {
		// no point in updating OCSP for expired certificates
		// TODO: ideally, cert.Leaf will never be nil, however, it currently is
		// when solving the TLS-ALPN challenge which adds a special cert directly
		// to the cache, since tls.X509KeyPair() discards the leaf
		if cert.Leaf == nil || time.Now().After(cert.Leaf.NotAfter) {
			continue
		}
		var lastNextUpdate time.Time
		if cert.ocsp != nil {
			lastNextUpdate = cert.ocsp.NextUpdate
			if freshOCSP(cert.ocsp) {
				continue // no need to update staple if ours is still fresh
			}
		}
		updateQueue = append(updateQueue, updateQueueEntry{cert, certHash, lastNextUpdate})
	}
	certCache.mu.RUnlock()

	// perform updates outside of any lock on certCache
	for _, qe := range updateQueue {
		cert := qe.cert
		certHash := qe.certHash
		lastNextUpdate := qe.lastNextUpdate

		cfg, err := certCache.getConfig(cert)
		if err != nil {
			log.Printf("[ERROR] Getting configuration to manage OCSP for certificate with names %v; unable to refresh: %v", cert.Names, err)
			continue
		}
		if cfg == nil {
			// this is bad if this happens, probably a programmer error (oops)
			log.Printf("[ERROR] No configuration associated with certificate for names %v; unable to manage OCSP", cert.Names)
			continue
		}

		ocspResp, err := stapleOCSP(cfg.Storage, &cert, nil)
		if err != nil {
			if cert.ocsp != nil {
				// if there was no staple before, that's fine; otherwise we should log the error
				log.Printf("[ERROR] Checking OCSP: %v", err)
			}
			continue
		}

		// By this point, we've obtained the latest OCSP response.
		// If there was no staple before, or if the response is updated, make
		// sure we apply the update to all names on the certificate.
		if cert.ocsp != nil && (lastNextUpdate.IsZero() || lastNextUpdate != cert.ocsp.NextUpdate) {
			log.Printf("[INFO] Advancing OCSP staple for %v from %s to %s",
				cert.Names, lastNextUpdate, cert.ocsp.NextUpdate)
			updated[certHash] = ocspUpdate{rawBytes: cert.Certificate.OCSPStaple, parsed: cert.ocsp}
		}

		// If a managed certificate was revoked, we should attempt
		// to replace it with a new one. If that fails, oh well.
		if cert.managed && ocspResp.Status == ocsp.Revoked && len(cert.Names) > 0 {
			renewQueue = append(renewQueue, cert)
			configs[cert.Names[0]] = cfg
		}
	}

	// These write locks should be brief since we have all the info we need now.
	for certKey, update := range updated {
		certCache.mu.Lock()
		cert := certCache.cache[certKey]
		cert.ocsp = update.parsed
		cert.Certificate.OCSPStaple = update.rawBytes
		certCache.cache[certKey] = cert
		certCache.mu.Unlock()
	}

	// We attempt to replace any certificates that were revoked.
	// Crucially, this happens OUTSIDE a lock on the certCache.
	for _, oldCert := range renewQueue {
		log.Printf("[INFO] OCSP status for managed certificate %v (expiration=%s) is REVOKED; attempting to replace with new certificate",
			oldCert.Names, oldCert.Leaf.NotAfter)

		renewName := oldCert.Names[0]
		cfg := configs[renewName]

		// TODO: consider using a new key in this situation, but we don't know if key storage has been compromised...
		err := cfg.RenewCert(ctx, renewName, false)
		if err != nil {
			// probably better to not serve a revoked certificate at all
			log.Printf("[ERROR] Obtaining new certificate for %v due to OCSP status of revoked: %v; removing from cache", oldCert.Names, err)
			certCache.mu.Lock()
			certCache.removeCertificate(oldCert)
			certCache.mu.Unlock()
			continue
		}
		err = cfg.reloadManagedCertificate(oldCert)
		if err != nil {
			log.Printf("[ERROR] After obtaining new certificate due to OCSP status of revoked: %v", err)
			continue
		}
	}
}

// CleanStorageOptions specifies how to clean up a storage unit.
type CleanStorageOptions struct {
	OCSPStaples            bool
	ExpiredCerts           bool
	ExpiredCertGracePeriod time.Duration
}

// CleanStorage removes assets which are no longer useful,
// according to opts.
func CleanStorage(storage Storage, opts CleanStorageOptions) {
	if opts.OCSPStaples {
		err := deleteOldOCSPStaples(storage)
		if err != nil {
			log.Printf("[ERROR] Deleting old OCSP staples: %v", err)
		}
	}
	if opts.ExpiredCerts {
		err := deleteExpiredCerts(storage, opts.ExpiredCertGracePeriod)
		if err != nil {
			log.Printf("[ERROR] Deleting expired certificates: %v", err)
		}
	}
	// TODO: delete stale locks?
}

func deleteOldOCSPStaples(storage Storage) error {
	ocspKeys, err := storage.List(prefixOCSP, false)
	if err != nil {
		// maybe just hasn't been created yet; no big deal
		return nil
	}
	for _, key := range ocspKeys {
		ocspBytes, err := storage.Load(key)
		if err != nil {
			log.Printf("[ERROR] While deleting old OCSP staples, unable to load staple file: %v", err)
			continue
		}
		resp, err := ocsp.ParseResponse(ocspBytes, nil)
		if err != nil {
			// contents are invalid; delete it
			err = storage.Delete(key)
			if err != nil {
				log.Printf("[ERROR] Purging corrupt staple file %s: %v", key, err)
			}
			continue
		}
		if time.Now().After(resp.NextUpdate) {
			// response has expired; delete it
			err = storage.Delete(key)
			if err != nil {
				log.Printf("[ERROR] Purging expired staple file %s: %v", key, err)
			}
		}
	}
	return nil
}

func deleteExpiredCerts(storage Storage, gracePeriod time.Duration) error {
	issuerKeys, err := storage.List(prefixCerts, false)
	if err != nil {
		// maybe just hasn't been created yet; no big deal
		return nil
	}

	for _, issuerKey := range issuerKeys {
		siteKeys, err := storage.List(issuerKey, false)
		if err != nil {
			log.Printf("[ERROR] Listing contents of %s: %v", issuerKey, err)
			continue
		}

		for _, siteKey := range siteKeys {
			siteAssets, err := storage.List(siteKey, false)
			if err != nil {
				log.Printf("[ERROR] Listing contents of %s: %v", siteKey, err)
				continue
			}

			for _, assetKey := range siteAssets {
				if path.Ext(assetKey) != ".crt" {
					continue
				}

				certFile, err := storage.Load(assetKey)
				if err != nil {
					return fmt.Errorf("loading certificate file %s: %v", assetKey, err)
				}
				block, _ := pem.Decode(certFile)
				if block == nil || block.Type != "CERTIFICATE" {
					return fmt.Errorf("certificate file %s does not contain PEM-encoded certificate", assetKey)
				}
				cert, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return fmt.Errorf("certificate file %s is malformed; error parsing PEM: %v", assetKey, err)
				}

				if expiredTime := time.Since(cert.NotAfter); expiredTime >= gracePeriod {
					log.Printf("[INFO] Certificate %s expired %s ago; cleaning up", assetKey, expiredTime)
					baseName := strings.TrimSuffix(assetKey, ".crt")
					for _, relatedAsset := range []string{
						assetKey,
						baseName + ".key",
						baseName + ".json",
					} {
						log.Printf("[INFO] Deleting %s because resource expired", relatedAsset)
						err := storage.Delete(relatedAsset)
						if err != nil {
							log.Printf("[ERROR] Cleaning up asset related to expired certificate for %s: %s: %v",
								baseName, relatedAsset, err)
						}
					}
				}
			}

			// update listing; if folder is empty, delete it
			siteAssets, err = storage.List(siteKey, false)
			if err != nil {
				continue
			}
			if len(siteAssets) == 0 {
				log.Printf("[INFO] Deleting %s because key is empty", siteKey)
				err := storage.Delete(siteKey)
				if err != nil {
					return fmt.Errorf("deleting empty site folder %s: %v", siteKey, err)
				}
			}
		}
	}
	return nil
}

const (
	// DefaultRenewCheckInterval is how often to check certificates for expiration.
	// Scans are very lightweight, so this can be semi-frequent. This default should
	// be smaller than <Minimum Cert Lifetime>*DefaultRenewalWindowRatio/3, which
	// gives certificates plenty of chance to be renewed on time.
	DefaultRenewCheckInterval = 10 * time.Minute

	// DefaultRenewalWindowRatio is how much of a certificate's lifetime becomes the
	// renewal window. The renewal window is the span of time at the end of the
	// certificate's validity period in which it should be renewed. A default value
	// of ~1/3 is pretty safe and recommended for most certificates.
	DefaultRenewalWindowRatio = 1.0 / 3.0

	// DefaultOCSPCheckInterval is how often to check if OCSP stapling needs updating.
	DefaultOCSPCheckInterval = 1 * time.Hour
)

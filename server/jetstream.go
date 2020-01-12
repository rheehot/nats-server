// Copyright 2019 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/nats-io/nats-server/v2/server/sysmem"
)

// JetStreamConfig determines this server's configuration.
// MaxMemory and MaxStore are in bytes.
type JetStreamConfig struct {
	MaxMemory int64
	MaxStore  int64
	StoreDir  string
}

// TODO(dlc) - need to track and rollup against server limits, etc.
type JetStreamAccountLimits struct {
	MaxMemory      int64 `json:"max_memory"`
	MaxStore       int64 `json:"max_storage"`
	MaxMsgSets     int   `json:"max_msg_sets"`
	MaxObservables int   `json:"max_observables"`
}

// JetStreamAccountStats returns current statistics about the account's JetStream usage.
type JetStreamAccountStats struct {
	Memory  uint64                 `json:"memory"`
	Store   uint64                 `json:"storage"`
	MsgSets int                    `json:"msg_sets"`
	Limits  JetStreamAccountLimits `json:"limits"`
}

// Responses to requests sent to a server from a client.
const (
	// OK response
	OK = "+OK"
	// ERR prefix response
	ErrPrefix = "-ERR"

	// JetStreamNotEnabled is returned when JetStream is not enabled.
	JetStreamNotEnabled = "-ERR 'jetstream not enabled for account'"
	// JetStreamBadRequest is returned when the request could not be properly parsed.
	JetStreamBadRequest = "-ERR 'bad request'"
)

// Request API for JetStream remote clients.
const (
	// JetStreamEnabled allows a user to dynamically check if JetStream is enabled for an account.
	// Will return +OK on success, otherwise will timeout.
	JetStreamEnabled = "$JS.ENABLED"
	jsEnabledExport  = "$JS.*.ENABLED"

	// JetStreamInfo is for obtaining general information about JetStream for this account.
	// Will return JSON response.
	JetStreamInfo = "$JS.INFO"
	jsInfoExport  = "$JS.*.INFO"

	// JetStreamCreateMsgSet is the endpoint to create new message sets.
	// Will return +OK on success and -ERR on failure.
	JetStreamCreateMsgSet = "$JS.MSGSET.CREATE"
	jsCreateMsgSetExport  = "$JS.*.MSGSET.CREATE"

	// JetStreamMsgSets is the endpoint to list all message sets for this account.
	// Will return json list of string on success and -ERR on failure.
	JetStreamMsgSets = "$JS.MSGSETS"
	jsMsgSetsExport  = "$JS.*.MSGSETS"

	// JetStreamMsgSetInfo is for obtaining general information about a named message set.
	// Will return JSON response.
	JetStreamMsgSetInfo = "$JS.MSGSET.INFO"
	jsMsgSetInfoExport  = "$JS.*.MSGSET.INFO"

	// JetStreamDeleteMsgSet is the endpoint to delete message sets.
	// Will return +OK on success and -ERR on failure.
	JetStreamDeleteMsgSet = "$JS.MSGSET.DELETE"
	jsDeleteMsgSetExport  = "$JS.*.MSGSET.DELETE"

	// JetStreamPurgeMsgSet is the endpoint to purge message sets.
	// Will return +OK on success and -ERR on failure.
	JetStreamPurgeMsgSet = "$JS.MSGSET.PURGE"
	jsPurgeMsgSetExport  = "$JS.*.MSGSET.PURGE"

	// JetStreamDeleteMsg is the endpoint to delete messages from a message set.
	// Will return +OK on success and -ERR on failure.
	JetStreamDeleteMsg = "$JS.MSGSET.MSG.DELETE"
	jsDeleteMsgExport  = "$JS.*.MSGSET.MSG.DELETE"

	// JetStreamCreateObservable is the endpoint to create observers for a message set.
	// Will return +OK on success and -ERR on failure.
	JetStreamCreateObservable = "$JS.OBSERVABLE.CREATE"
	jsCreateObservableExport  = "$JS.*.OBSERVABLE.CREATE"

	// JetStreamObservables is the endpoint to list all observables for the message set.
	// Will return json list of string on success and -ERR on failure.
	JetStreamObservables = "$JS.OBSERVABLES"
	jsObservablesExport  = "$JS.*.OBSERVABLES"

	// JsObservableInfo is for obtaining general information about an observable.
	// Will return JSON response.
	JetStreamObservableInfo = "$JS.OBSERVABLE.INFO"
	jsObservableInfoExport  = "$JS.*.OBSERVABLE.INFO"

	// JetStreamDeleteObservable is the endpoint to delete observables.
	// Will return +OK on success and -ERR on failure.
	JetStreamDeleteObservable = "$JS.OBSERVABLE.DELETE"
	jsDeleteObservableExport  = "$JS.*.OBSERVABLE.DELETE"

	// JetStreamAckPre is the prefix for the ack stream coming back to an observable.
	JetStreamAckPre = "$JS.A"

	// JetStreamRequestNextPre is the prefix for the request next message(s) for an observable in worker/pull mode.
	JetStreamRequestNextPre = "$JS.RN"

	// JetStreamMsgBySeq is the prefix for direct requests for a message by message set sequence number
	JetStreamMsgBySeqPre = "$JS.BYSEQ"

	// JetStreamObservableAckSamplePre is the prefix for sample messages from Observables when messages are acknowledged
	JetStreamObservableAckSamplePre = "$JS.EVENTS.OBSERVABLE.ACKSAMPLE"

	// JetStreamObservableMaxDeliverExceedSamplePre is the prefix for an event published when a Observable stops delivering a message due to maximum deliveries
	JetStreamObservableMaxDeliverExceedSamplePre = "$JS.EVENTS.OBSERVABLE.MAXDELIVER"
)

// This is for internal accounting for JetStream for this server.
type jetStream struct {
	mu            sync.RWMutex
	srv           *Server
	config        JetStreamConfig
	accounts      map[*Account]*jsAccount
	memReserved   int64
	storeReserved int64
}

// Metafiles
const (
	// Metafiles for message sets and observables.
	JetStreamMetaFile    = "meta.inf"
	JetStreamMetaFileSum = "meta.sum"
)

// For easier handling of exports and imports.
var allJsExports = []string{
	jsEnabledExport,
	jsInfoExport,
	jsCreateMsgSetExport,
	jsMsgSetsExport,
	jsMsgSetInfoExport,
	jsDeleteMsgSetExport,
	jsPurgeMsgSetExport,
	jsDeleteMsgExport,
	jsCreateObservableExport,
	jsObservablesExport,
	jsObservableInfoExport,
	jsDeleteObservableExport,
}

// This represents a jetstream  enabled account.
// Worth noting that we include the js ptr, this is because
// in general we want to be very efficient when receiving messages on
// and internal sub for a msgSet, so we will direct link to the msgSet
// and walk backwards as needed vs multiple hash lookups and locks, etc.
type jsAccount struct {
	mu            sync.RWMutex
	js            *jetStream
	account       *Account
	limits        JetStreamAccountLimits
	memReserved   int64
	memUsed       int64
	storeReserved int64
	storeUsed     int64
	storeDir      string
	msgSets       map[string]*MsgSet
}

// EnableJetStream will enable JetStream support on this server with the given configuration.
// A nil configuration will dynamically choose the limits and temporary file storage directory.
// If this server is part of a cluster, a system account will need to be defined.
func (s *Server) EnableJetStream(config *JetStreamConfig) error {
	s.mu.Lock()
	if !s.standAloneMode() {
		s.mu.Unlock()
		return fmt.Errorf("jetstream restricted to single server mode")
	}
	if s.js != nil {
		s.mu.Unlock()
		return fmt.Errorf("jetstream already enabled")
	}
	s.Noticef("Starting JetStream")
	if config == nil || config.MaxMemory <= 0 || config.MaxStore <= 0 {
		var storeDir string
		s.Debugf("JetStream creating dynamic configuration - 75%% of system memory, %s disk", FriendlyBytes(JetStreamMaxStoreDefault))
		if config != nil {
			storeDir = config.StoreDir
		}
		config = s.dynJetStreamConfig(storeDir)
	}
	// Copy, don't change callers.
	cfg := *config
	if cfg.StoreDir == "" {
		cfg.StoreDir = filepath.Join(os.TempDir(), JetStreamStoreDir)
	}

	s.js = &jetStream{srv: s, config: cfg, accounts: make(map[*Account]*jsAccount)}
	s.mu.Unlock()

	if stat, err := os.Stat(cfg.StoreDir); os.IsNotExist(err) {
		if err := os.MkdirAll(cfg.StoreDir, 0755); err != nil {
			return fmt.Errorf("could not create storage directory - %v", err)
		}
	} else {
		// Make sure its a directory and that we can write to it.
		if stat == nil || !stat.IsDir() {
			return fmt.Errorf("storage directory is not a directory")
		}
		tmpfile, err := ioutil.TempFile(cfg.StoreDir, "_test_")
		if err != nil {
			return fmt.Errorf("storage directory is not writable")
		}
		os.Remove(tmpfile.Name())
	}

	// JetStream is an internal service so we need to make sure we have a system account.
	// This system account will export the JetStream service endpoints.
	if sacc := s.SystemAccount(); sacc == nil {
		s.SetDefaultSystemAccount()
	}

	// Setup our internal subscriptions.
	if _, err := s.sysSubscribe(jsEnabledExport, s.isJsEnabledRequest); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}
	if _, err := s.sysSubscribe(jsInfoExport, s.jsAccountInfoRequest); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}
	if _, err := s.sysSubscribe(jsCreateMsgSetExport, s.jsCreateMsgSetRequest); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}
	if _, err := s.sysSubscribe(jsMsgSetsExport, s.jsMsgSetsRequest); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}
	if _, err := s.sysSubscribe(jsMsgSetInfoExport, s.jsMsgSetInfoRequest); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}
	if _, err := s.sysSubscribe(jsDeleteMsgSetExport, s.jsMsgSetDeleteRequest); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}
	if _, err := s.sysSubscribe(jsPurgeMsgSetExport, s.jsMsgSetPurgeRequest); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}
	if _, err := s.sysSubscribe(jsDeleteMsgExport, s.jsMsgDeleteRequest); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}
	if _, err := s.sysSubscribe(jsCreateObservableExport, s.jsCreateObservableRequest); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}
	if _, err := s.sysSubscribe(jsObservablesExport, s.jsObservablesRequest); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}
	if _, err := s.sysSubscribe(jsObservableInfoExport, s.jsObservableInfoRequest); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}
	if _, err := s.sysSubscribe(jsDeleteObservableExport, s.jsObservableDeleteRequest); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}

	s.Noticef("----------- JETSTREAM (Beta) -----------")
	s.Noticef("  Max Memory:      %s", FriendlyBytes(cfg.MaxMemory))
	s.Noticef("  Max Storage:     %s", FriendlyBytes(cfg.MaxStore))
	s.Noticef("  Store Directory: %q", cfg.StoreDir)

	// Setup our internal system exports.
	sacc := s.SystemAccount()
	// FIXME(dlc) - Should we lock these down?
	s.Debugf("  Exports:")
	for _, export := range allJsExports {
		s.Debugf("     %s", export)
		if err := sacc.AddServiceExport(export, nil); err != nil {
			return fmt.Errorf("Error setting up jetstream service exports: %v", err)
		}
	}
	s.Noticef("----------------------------------------")

	// If we have no configured accounts setup then setup imports on global account.
	if s.globalAccountOnly() {
		if err := s.GlobalAccount().EnableJetStream(nil); err != nil {
			return fmt.Errorf("Error enabling jetstream on the global account")
		}
	}

	return nil
}

// JetStreamEnabled reports if jetstream is enabled.
func (s *Server) JetStreamEnabled() bool {
	s.mu.Lock()
	enabled := s.js != nil
	s.mu.Unlock()
	return enabled
}

// Shutdown jetstream for this server.
func (s *Server) shutdownJetStream() {
	s.mu.Lock()
	if s.js == nil {
		s.mu.Unlock()
		return
	}
	var _jsa [512]*jsAccount
	jsas := _jsa[:0]
	// Collect accounts.
	for _, jsa := range s.js.accounts {
		jsas = append(jsas, jsa)
	}
	s.mu.Unlock()

	for _, jsa := range jsas {
		jsa.flushState()
		s.js.disableJetStream(jsa)
	}

	s.mu.Lock()
	s.js.accounts = nil
	s.js = nil
	s.mu.Unlock()
}

// JetStreamConfig will return the current config. Useful if the system
// created a dynamic configuration. A copy is returned.
func (s *Server) JetStreamConfig() *JetStreamConfig {
	var c *JetStreamConfig
	s.mu.Lock()
	if s.js != nil {
		copy := s.js.config
		c = &(copy)
	}
	s.mu.Unlock()
	return c
}

// JetStreamNumAccounts returns the number of enabled accounts this server is tracking.
func (s *Server) JetStreamNumAccounts() int {
	js := s.getJetStream()
	if js == nil {
		return 0
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	return len(js.accounts)
}

// JetStreamReservedResources returns the reserved resources if JetStream is enabled.
func (s *Server) JetStreamReservedResources() (int64, int64, error) {
	js := s.getJetStream()
	if js == nil {
		return -1, -1, fmt.Errorf("jetstream not enabled")
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return js.memReserved, js.storeReserved, nil
}

func (s *Server) getJetStream() *jetStream {
	s.mu.Lock()
	js := s.js
	s.mu.Unlock()
	return js
}

// EnableJetStream will enable JetStream on this account with the defined limits.
// This is a helper for JetStreamEnableAccount.
func (a *Account) EnableJetStream(limits *JetStreamAccountLimits) error {
	a.mu.RLock()
	s := a.srv
	a.mu.RUnlock()
	if s == nil {
		return fmt.Errorf("jetstream account not registered")
	}
	// FIXME(dlc) - cluster mode
	js := s.getJetStream()
	if js == nil {
		return fmt.Errorf("jetstream not enabled")
	}

	// No limits means we dynamically set up limits.
	if limits == nil {
		limits = js.dynamicAccountLimits()
	}

	js.mu.Lock()
	// Check the limits against existing reservations.
	if err := js.sufficientResources(limits); err != nil {
		js.mu.Unlock()
		return err
	}
	if _, ok := js.accounts[a]; ok {
		js.mu.Unlock()
		return fmt.Errorf("jetstream already enabled for account")
	}
	jsa := &jsAccount{js: js, account: a, limits: *limits, msgSets: make(map[string]*MsgSet)}
	jsa.storeDir = path.Join(js.config.StoreDir, a.Name)
	js.accounts[a] = jsa
	js.reserveResources(limits)
	js.mu.Unlock()

	// Stamp inside account as well.
	a.mu.Lock()
	a.js = jsa
	a.mu.Unlock()

	// Create the proper imports here.
	sys := s.SystemAccount()
	for _, export := range allJsExports {
		importTo := strings.Replace(export, "*", a.Name, -1)
		importFrom := strings.Replace(export, ".*.", tsep, -1)
		if err := a.AddServiceImport(sys, importFrom, importTo); err != nil {
			return fmt.Errorf("Error setting up jetstream service imports for account: %v", err)
		}
	}

	s.Debugf("Enabled JetStream for account %q", a.Name)
	s.Debugf("  Max Memory:      %s", FriendlyBytes(limits.MaxMemory))
	s.Debugf("  Max Storage:     %s", FriendlyBytes(limits.MaxStore))

	// Restore any state here.
	fis, _ := ioutil.ReadDir(jsa.storeDir)
	if len(fis) > 0 {
		s.Noticef("  Recovering JetStream state for account %q", a.Name)
	}
	for _, fi := range fis {
		metafile := path.Join(jsa.storeDir, fi.Name(), JetStreamMetaFile)
		metasum := path.Join(jsa.storeDir, fi.Name(), JetStreamMetaFileSum)
		if _, err := os.Stat(metafile); os.IsNotExist(err) {
			s.Warnf("  Missing MsgSet metafile for %q", metafile)
			continue
		}
		buf, err := ioutil.ReadFile(metafile)
		if err != nil {
			s.Warnf("  Error reading metafile %q: %v", metasum, err)
			continue
		}
		if _, err := os.Stat(metasum); os.IsNotExist(err) {
			s.Warnf("  Missing MsgSet checksum for %q", metasum)
			continue
		}
		// FIXME(dlc) - check checksum.
		var cfg MsgSetConfig
		if err := json.Unmarshal(buf, &cfg); err != nil {
			s.Warnf("  Error unmarshalling MsgSet metafile: %v", err)
			continue
		}
		mset, err := a.AddMsgSet(&cfg)
		if err != nil {
			s.Warnf("  Error recreating MsgSet: %v", err)
		}

		stats := mset.Stats()
		s.Noticef("  Restored %d messages for MsgSet %q", comma(int64(stats.Msgs)), fi.Name())

		// Now do Observables.
		odir := path.Join(jsa.storeDir, fi.Name(), obsDir)
		ofis, _ := ioutil.ReadDir(odir)
		if len(ofis) > 0 {
			s.Noticef("  Recovering %d Observables for MsgSet - %q", len(ofis), fi.Name())
		}
		for _, ofi := range ofis {
			metafile := path.Join(odir, ofi.Name(), JetStreamMetaFile)
			metasum := path.Join(odir, ofi.Name(), JetStreamMetaFileSum)
			if _, err := os.Stat(metafile); os.IsNotExist(err) {
				s.Warnf("    Missing Observable Metafile %q", metafile)
				continue
			}
			buf, err := ioutil.ReadFile(metafile)
			if err != nil {
				s.Warnf("    Error reading observable metafile %q: %v", metasum, err)
				continue
			}
			if _, err := os.Stat(metasum); os.IsNotExist(err) {
				s.Warnf("    Missing Observable checksum for %q", metasum)
				continue
			}
			var cfg ObservableConfig
			if err := json.Unmarshal(buf, &cfg); err != nil {
				s.Warnf("    Error unmarshalling Observable metafile: %v", err)
				continue
			}
			obs, err := mset.AddObservable(&cfg)
			if err != nil {
				s.Warnf("    Error adding Observable: %v", err)
				continue
			}
			if err := obs.readStoredState(); err != nil {
				s.Warnf("    Error restoring Observable state: %v", err)
			}
		}
	}

	s.Noticef("JetStream state for account %q recovered", a.Name)

	return nil
}

// MsgSets will return all known message sets.
func (a *Account) MsgSets() []*MsgSet {
	a.mu.RLock()
	jsa := a.js
	a.mu.RUnlock()
	if jsa == nil {
		return nil
	}
	var msets []*MsgSet
	jsa.mu.Lock()
	for _, mset := range jsa.msgSets {
		msets = append(msets, mset)
	}
	jsa.mu.Unlock()
	return msets
}

func (a *Account) LookupMsgSet(name string) (*MsgSet, error) {
	a.mu.RLock()
	jsa := a.js
	a.mu.RUnlock()

	if jsa == nil {
		return nil, fmt.Errorf("jetstream not enabled")
	}
	jsa.mu.Lock()
	mset, ok := jsa.msgSets[name]
	jsa.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("msgset not found")
	}
	return mset, nil
}

// UpdateJetStreamLimits will update the account limits for a JetStream enabled account.
func (a *Account) UpdateJetStreamLimits(limits *JetStreamAccountLimits) error {
	a.mu.RLock()
	s := a.srv
	a.mu.RUnlock()
	if s == nil {
		return fmt.Errorf("jetstream account not registered")
	}

	js := s.getJetStream()
	if js == nil {
		return fmt.Errorf("jetstream not enabled")
	}

	jsa := js.lookupAccount(a)
	if jsa == nil {
		return fmt.Errorf("jetstream not enabled for account")
	}

	if limits == nil {
		limits = js.dynamicAccountLimits()
	}

	// Calculate the delta between what we have and what we want.
	jsa.mu.Lock()
	dl := diffCheckedLimits(&jsa.limits, limits)
	jsaLimits := jsa.limits
	jsa.mu.Unlock()

	js.mu.Lock()
	// Check the limits against existing reservations.
	if err := js.sufficientResources(&dl); err != nil {
		js.mu.Unlock()
		return err
	}
	// FIXME(dlc) - If we drop and are over the max on memory or store, do we delete??
	js.releaseResources(&jsaLimits)
	js.reserveResources(limits)
	js.mu.Unlock()

	// Update
	jsa.mu.Lock()
	jsa.limits = *limits
	jsa.mu.Unlock()

	return nil
}

func diffCheckedLimits(a, b *JetStreamAccountLimits) JetStreamAccountLimits {
	return JetStreamAccountLimits{
		MaxMemory: b.MaxMemory - a.MaxMemory,
		MaxStore:  b.MaxStore - a.MaxStore,
	}
}

// JetStreamUsage reports on JetStream usage and limits for an account.
func (a *Account) JetStreamUsage() JetStreamAccountStats {
	a.mu.RLock()
	jsa := a.js
	a.mu.RUnlock()

	var stats JetStreamAccountStats
	if jsa != nil {
		jsa.mu.Lock()
		stats.Memory = uint64(jsa.memUsed)
		stats.Store = uint64(jsa.storeUsed)
		stats.MsgSets = len(jsa.msgSets)
		stats.Limits = jsa.limits
		jsa.mu.Unlock()
	}
	return stats
}

// DisableJetStream will disable JetStream for this account.
func (a *Account) DisableJetStream() error {
	a.mu.Lock()
	s := a.srv
	a.js = nil
	a.mu.Unlock()

	if s == nil {
		return fmt.Errorf("jetstream account not registered")
	}

	js := s.getJetStream()
	if js == nil {
		return fmt.Errorf("jetstream not enabled")
	}

	// Remove service imports.
	for _, export := range allJsExports {
		from := strings.Replace(export, ".*.", tsep, -1)
		a.removeServiceImport(from)
	}

	return js.disableJetStream(js.lookupAccount(a))
}

// Disable JetStream for the account.
func (js *jetStream) disableJetStream(jsa *jsAccount) error {
	if jsa == nil {
		return fmt.Errorf("jetstream not enabled for account")
	}

	js.mu.Lock()
	delete(js.accounts, jsa.account)
	js.releaseResources(&jsa.limits)
	js.mu.Unlock()

	jsa.delete()

	return nil
}

// Flush JetStream state for the account.
func (jsa *jsAccount) flushState() error {
	if jsa == nil {
		return fmt.Errorf("jetstream not enabled for account")
	}

	// Collect the message sets.
	var _msets [64]*MsgSet
	msets := _msets[:0]
	jsa.mu.Lock()
	for _, mset := range jsa.msgSets {
		msets = append(msets, mset)
	}
	jsa.mu.Unlock()

	for _, mset := range msets {
		mset.store.Stop()
	}
	return nil
}

// JetStreamEnabled is a helper to determine if jetstream is enabled for an account.
func (a *Account) JetStreamEnabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	enabled := a.js != nil
	a.mu.RUnlock()
	return enabled
}

// Updates accounting on in use memory and storage.
func (jsa *jsAccount) updateUsage(storeType StorageType, delta int64) {
	// TODO(dlc) - atomics? snapshot limits?
	jsa.mu.Lock()
	if storeType == MemoryStorage {
		jsa.memUsed += delta
	} else {
		jsa.storeUsed += delta
	}
	jsa.mu.Unlock()
}

func (jsa *jsAccount) limitsExceeded(storeType StorageType) bool {
	var exceeded bool
	jsa.mu.Lock()
	if storeType == MemoryStorage {
		if jsa.memUsed > jsa.limits.MaxMemory {
			exceeded = true
		}
	} else {
		if jsa.storeUsed > jsa.limits.MaxStore {
			exceeded = true
		}
	}
	jsa.mu.Unlock()
	return exceeded
}

// Check if a new proposed msg set while exceed our account limits.
// Lock should be held.
func (jsa *jsAccount) checkLimits(config *MsgSetConfig) error {
	if jsa.limits.MaxMsgSets > 0 && len(jsa.msgSets) >= jsa.limits.MaxMsgSets {
		return fmt.Errorf("maximum number of message sets reached")
	}
	// FIXME(dlc) - Add check here for replicas based on clustering.
	if config.Replicas != 1 {
		return fmt.Errorf("replicas setting of %d not allowed", config.Replicas)
	}
	// Check MaxObservables
	if config.MaxObservables > 0 && config.MaxObservables > jsa.limits.MaxObservables {
		return fmt.Errorf("maximum observables exceeds account limit")
	} else {
		config.MaxObservables = jsa.limits.MaxObservables
	}
	// Check storage, memory or disk.
	if config.MaxBytes > 0 {
		mb := config.MaxBytes * int64(config.Replicas)
		switch config.Storage {
		case MemoryStorage:
			if jsa.memReserved+mb > jsa.limits.MaxMemory {
				return fmt.Errorf("insufficient memory resources available")
			}
		case FileStorage:
			if jsa.storeReserved+mb > jsa.limits.MaxStore {
				return fmt.Errorf("insufficient storage resources available")
			}
		}
	}
	return nil
}

// Delete the JetStream resources.
func (jsa *jsAccount) delete() {
	var msgSets []*MsgSet
	jsa.mu.Lock()
	for _, ms := range jsa.msgSets {
		msgSets = append(msgSets, ms)
	}
	jsa.mu.Unlock()
	for _, ms := range msgSets {
		ms.stop(false)
	}
}

// Lookup the jetstream account for a given account.
func (js *jetStream) lookupAccount(a *Account) *jsAccount {
	js.mu.RLock()
	jsa := js.accounts[a]
	js.mu.RUnlock()
	return jsa
}

// Will dynamically create limits for this account.
func (js *jetStream) dynamicAccountLimits() *JetStreamAccountLimits {
	js.mu.RLock()
	// For now used all resources. Mostly meant for $G in non-account mode.
	limits := &JetStreamAccountLimits{js.config.MaxMemory, js.config.MaxStore, -1, -1}
	js.mu.RUnlock()
	return limits
}

// Check to see if we have enough system resources for this account.
// Lock should be held.
func (js *jetStream) sufficientResources(limits *JetStreamAccountLimits) error {
	if limits == nil {
		return nil
	}
	if js.memReserved+limits.MaxMemory > js.config.MaxMemory {
		return fmt.Errorf("insufficient memory resources available")
	}
	if js.storeReserved+limits.MaxStore > js.config.MaxStore {
		return fmt.Errorf("insufficient storage resources available")
	}
	return nil
}

// This will (blindly) reserve the respources requested.
// Lock should be held.
func (js *jetStream) reserveResources(limits *JetStreamAccountLimits) error {
	if limits == nil {
		return nil
	}
	if limits.MaxMemory > 0 {
		js.memReserved += limits.MaxMemory
	}
	if limits.MaxStore > 0 {
		js.storeReserved += limits.MaxStore
	}
	return nil
}

func (js *jetStream) releaseResources(limits *JetStreamAccountLimits) error {
	if limits == nil {
		return nil
	}
	if limits.MaxMemory > 0 {
		js.memReserved -= limits.MaxMemory
	}
	if limits.MaxStore > 0 {
		js.storeReserved -= limits.MaxStore
	}
	return nil
}

// Request to check if jetstream is enabled.
func (s *Server) isJsEnabledRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	if c == nil || c.acc == nil {
		return
	}
	if c.acc.JetStreamEnabled() {
		s.sendInternalAccountMsg(c.acc, reply, OK)
	} else {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamNotEnabled)
	}
}

// Request for current usage and limits for this account.
func (s *Server) jsAccountInfoRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	if c == nil || c.acc == nil {
		return
	}
	if !c.acc.JetStreamEnabled() {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamNotEnabled)
		return
	}
	stats := c.acc.JetStreamUsage()
	b, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return
	}
	s.sendInternalAccountMsg(c.acc, reply, b)
}

// Request to create a message set.
func (s *Server) jsCreateMsgSetRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	if c == nil || c.acc == nil {
		return
	}
	if !c.acc.JetStreamEnabled() {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamNotEnabled)
		return
	}
	var cfg MsgSetConfig
	if err := json.Unmarshal(msg, &cfg); err != nil {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamBadRequest)
		return
	}
	var response = OK
	if _, err := c.acc.AddMsgSet(&cfg); err != nil {
		response = fmt.Sprintf("%s %v", ErrPrefix, err)
	}
	s.sendInternalAccountMsg(c.acc, reply, response)
}

// Request for the list of all message sets.
func (s *Server) jsMsgSetsRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	if c == nil || c.acc == nil {
		return
	}
	if !c.acc.JetStreamEnabled() {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamNotEnabled)
		return
	}
	var names []string
	msets := c.acc.MsgSets()
	for _, mset := range msets {
		names = append(names, mset.Name())
	}
	b, err := json.MarshalIndent(names, "", "  ")
	if err != nil {
		return
	}
	s.sendInternalAccountMsg(c.acc, reply, b)
}

// Request for information about a message set.
// This expects a message set name as the msg body.
func (s *Server) jsMsgSetInfoRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	if c == nil || c.acc == nil {
		return
	}
	if !c.acc.JetStreamEnabled() {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamNotEnabled)
		return
	}
	if len(msg) == 0 {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamBadRequest)
		return
	}
	mset, err := c.acc.LookupMsgSet(string(msg))
	if err != nil {
		s.sendInternalAccountMsg(c.acc, reply, fmt.Sprintf("%s '%v'", ErrPrefix, err))
		return
	}
	msi := MsgSetInfo{
		Stats:  mset.Stats(),
		Config: mset.Config(),
	}
	b, err := json.MarshalIndent(msi, "", "  ")
	if err != nil {
		return
	}
	s.sendInternalAccountMsg(c.acc, reply, b)
}

// Request to delete a message set.
// This expects a message set name as the msg body.
func (s *Server) jsMsgSetDeleteRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	if c == nil || c.acc == nil {
		return
	}
	if !c.acc.JetStreamEnabled() {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamNotEnabled)
		return
	}
	if len(msg) == 0 {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamBadRequest)
		return
	}
	mset, err := c.acc.LookupMsgSet(string(msg))
	if err != nil {
		s.sendInternalAccountMsg(c.acc, reply, fmt.Sprintf("%s %v", ErrPrefix, err))
		return
	}
	var response = OK
	if err := mset.Delete(); err != nil {
		response = fmt.Sprintf("%s %v", ErrPrefix, err)
	}
	s.sendInternalAccountMsg(c.acc, reply, response)
}

// Request to delete a message.
// This expects a message set name and store sequence number as the msg body.
func (s *Server) jsMsgDeleteRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	if c == nil || c.acc == nil {
		return
	}
	if !c.acc.JetStreamEnabled() {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamNotEnabled)
		return
	}
	args := strings.Split(string(msg), " ")
	if len(args) != 2 {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamBadRequest)
		return
	}
	name := args[0]
	seq, _ := strconv.Atoi(args[1])

	mset, err := c.acc.LookupMsgSet(name)
	if err != nil {
		s.sendInternalAccountMsg(c.acc, reply, fmt.Sprintf("%s %v", ErrPrefix, err))
		return
	}
	var response = OK
	if !mset.EraseMsg(uint64(seq)) {
		response = fmt.Sprintf("%s sequence [%d] not found", ErrPrefix, seq)
	}
	s.sendInternalAccountMsg(c.acc, reply, response)
}

// Request to purge a message set.
// This expects a message set name as the msg body.
func (s *Server) jsMsgSetPurgeRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	if c == nil || c.acc == nil {
		return
	}
	if !c.acc.JetStreamEnabled() {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamNotEnabled)
		return
	}
	if len(msg) == 0 {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamBadRequest)
		return
	}
	mset, err := c.acc.LookupMsgSet(string(msg))
	if err != nil {
		s.sendInternalAccountMsg(c.acc, reply, fmt.Sprintf("%s %v", ErrPrefix, err))
		return
	}

	mset.Purge()
	s.sendInternalAccountMsg(c.acc, reply, OK)
}

// Request to create an observable.
func (s *Server) jsCreateObservableRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	if c == nil || c.acc == nil {
		return
	}
	if !c.acc.JetStreamEnabled() {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamNotEnabled)
		return
	}
	var req CreateObservableRequest
	if err := json.Unmarshal(msg, &req); err != nil {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamBadRequest)
		return
	}
	mset, err := c.acc.LookupMsgSet(string(req.MsgSet))
	if err != nil {
		s.sendInternalAccountMsg(c.acc, reply, fmt.Sprintf("%s %v", ErrPrefix, err))
		return
	}
	var response = OK
	if _, err := mset.AddObservable(&req.Config); err != nil {
		response = fmt.Sprintf("%s '%v'", ErrPrefix, err)
	}
	s.sendInternalAccountMsg(c.acc, reply, response)
}

// Request for the list of all observables.
// This expects a message set name as the msg body.
func (s *Server) jsObservablesRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	if c == nil || c.acc == nil {
		return
	}
	if !c.acc.JetStreamEnabled() {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamNotEnabled)
		return
	}
	if len(msg) == 0 {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamBadRequest)
		return
	}
	mset, err := c.acc.LookupMsgSet(string(msg))
	if err != nil {
		s.sendInternalAccountMsg(c.acc, reply, fmt.Sprintf("%s %v", ErrPrefix, err))
		return
	}
	var onames []string
	obs := mset.Observables()
	for _, o := range obs {
		onames = append(onames, o.Name())
	}
	b, err := json.MarshalIndent(onames, "", "  ")
	if err != nil {
		return
	}
	s.sendInternalAccountMsg(c.acc, reply, b)
}

// Request for information about an observable.
// This expects a message set name and observable name as the msg body. e.g. "MSGSET1 OBS1"
func (s *Server) jsObservableInfoRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	if c == nil || c.acc == nil {
		return
	}
	if !c.acc.JetStreamEnabled() {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamNotEnabled)
		return
	}
	if len(msg) == 0 {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamBadRequest)
		return
	}
	names := strings.Split(string(msg), " ")
	if len(names) != 2 {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamBadRequest)
		return
	}
	mset, err := c.acc.LookupMsgSet(names[0])
	if err != nil {
		s.sendInternalAccountMsg(c.acc, reply, fmt.Sprintf("%s %v", ErrPrefix, err))
		return
	}
	obs := mset.LookupObservable(names[1])
	if obs == nil {
		s.sendInternalAccountMsg(c.acc, reply, fmt.Sprintf("%s observable not found", ErrPrefix))
		return
	}
	info := obs.Info()
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return
	}
	s.sendInternalAccountMsg(c.acc, reply, b)
}

// Request to delete an Observable.
// This expects a message set name and observable name as the msg body. e.g. "MSGSET1 OBS1"
func (s *Server) jsObservableDeleteRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	if c == nil || c.acc == nil {
		return
	}
	if !c.acc.JetStreamEnabled() {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamNotEnabled)
		return
	}
	if len(msg) == 0 {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamBadRequest)
		return
	}
	names := strings.Split(string(msg), " ")
	if len(names) != 2 {
		s.sendInternalAccountMsg(c.acc, reply, JetStreamBadRequest)
		return
	}
	mset, err := c.acc.LookupMsgSet(names[0])
	if err != nil {
		s.sendInternalAccountMsg(c.acc, reply, fmt.Sprintf("%s %v", ErrPrefix, err))
		return
	}
	obs := mset.LookupObservable(names[1])
	if obs == nil {
		s.sendInternalAccountMsg(c.acc, reply, fmt.Sprintf("%s observable not found", ErrPrefix))
		return
	}
	var response = OK
	if err := obs.Delete(); err != nil {
		response = fmt.Sprintf("%s %v", ErrPrefix, err)
	}
	s.sendInternalAccountMsg(c.acc, reply, response)

}

const (
	// JetStreamStoreDir is the prefix we use.
	JetStreamStoreDir = "jetstream"
	// JetStreamMaxStoreDefault is the default disk storage limit. 1TB
	JetStreamMaxStoreDefault = 1024 * 1024 * 1024 * 1024
	// JetStreamMaxMemDefault is only used when we can't determine system memory. 256MB
	JetStreamMaxMemDefault = 1024 * 1024 * 256
)

// Dynamically create a config with a tmp based directory (repeatable) and 75% of system memory.
func (s *Server) dynJetStreamConfig(storeDir string) *JetStreamConfig {
	jsc := &JetStreamConfig{}
	if storeDir != "" {
		jsc.StoreDir = filepath.Join(storeDir, JetStreamStoreDir)
	} else {
		jsc.StoreDir = filepath.Join(os.TempDir(), JetStreamStoreDir)
	}
	jsc.MaxStore = JetStreamMaxStoreDefault
	// Estimate to 75% of total memory if we can determine system memory.
	if sysMem := sysmem.Memory(); sysMem > 0 {
		jsc.MaxMemory = sysMem / 4 * 3
	} else {
		jsc.MaxMemory = JetStreamMaxMemDefault
	}
	return jsc
}

// FriendlyBytes returns a string with the given bytes int64
// represented as a size, such as 1KB, 10MB, etc...
func FriendlyBytes(bytes int64) string {
	fbytes := float64(bytes)
	base := 1024
	pre := []string{"K", "M", "G", "T", "P", "E"}
	if fbytes < float64(base) {
		return fmt.Sprintf("%v B", fbytes)
	}
	exp := int(math.Log(fbytes) / math.Log(float64(base)))
	index := exp - 1
	return fmt.Sprintf("%.2f %sB", fbytes/math.Pow(float64(base), float64(exp)), pre[index])
}

func isValidName(name string) bool {
	if name == "" {
		return false
	}
	return !strings.ContainsAny(name, ".*>")
}

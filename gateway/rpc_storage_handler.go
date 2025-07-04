//nolint:revive
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	temporalmodel "github.com/TykTechnologies/storage/temporal/model"
	"github.com/TykTechnologies/tyk/internal/cache"
	"github.com/TykTechnologies/tyk/internal/model"
	"github.com/TykTechnologies/tyk/rpc"

	"github.com/TykTechnologies/tyk/storage"

	"github.com/sirupsen/logrus"
)

var (
	dispatcherFuncs = map[string]interface{}{
		"Login": func(clientAddr, userKey string) bool {
			return false
		},
		"LoginWithGroup": func(clientAddr string, groupData *model.GroupLoginRequest) bool {
			return false
		},
		"GetKey": func(keyName string) (string, error) {
			return "", nil
		},
		"SetKey": func(ibd *model.InboundData) error {
			return nil
		},
		"GetExp": func(keyName string) (int64, error) {
			return 0, nil
		},
		"GetKeys": func(keyName string) ([]string, error) {
			return nil, nil
		},
		"DeleteKey": func(keyName string) (bool, error) {
			return true, nil
		},
		"DeleteRawKey": func(keyName string) (bool, error) {
			return true, nil
		},
		"GetKeysAndValues": func(searchString string) (*model.KeysValuesPair, error) {
			return nil, nil
		},
		"GetKeysAndValuesWithFilter": func(searchString string) (*model.KeysValuesPair, error) {
			return nil, nil
		},
		"DeleteKeys": func(keys []string) (bool, error) {
			return true, nil
		},
		"DeleteRawKeys": func(keys []string) (bool, error) {
			return true, nil
		},
		"Decrement": func(keyName string) error {
			return nil
		},
		"IncrememntWithExpire": func(ibd *model.InboundData) (int64, error) {
			return 0, nil
		},
		"AppendToSet": func(ibd *model.InboundData) error {
			return nil
		},
		"SetRollingWindow": func(ibd *model.InboundData) (int, error) {
			return 0, nil
		},
		"GetApiDefinitions": func(dr *model.DefRequest) (string, error) {
			return "", nil
		},
		"GetPolicies": func(orgId string) (string, error) {
			return "", nil
		},
		"PurgeAnalyticsData": func(data string) error {
			return nil
		},
		"CheckReload": func(clientAddr, orgId string) (bool, error) {
			return false, nil
		},
		"GetKeySpaceUpdate": func(clientAddr, orgId string) ([]string, error) {
			return nil, nil
		},
		"GetGroupKeySpaceUpdate": func(clientAddr string, groupData *model.GroupKeySpaceRequest) ([]string, error) {
			return nil, nil
		},
		"Ping": func() bool {
			return false
		},
		"Disconnect": func(clientAddr string, groupData *model.GroupLoginRequest) error {
			return nil
		},
	}
)

const (
	ResetQuota              string = "resetQuota"
	CertificateRemoved      string = "CertificateRemoved"
	CertificateAdded        string = "CertificateAdded"
	OAuthRevokeToken        string = "oAuthRevokeToken"
	OAuthRevokeAccessToken  string = "oAuthRevokeAccessToken"
	OAuthRevokeRefreshToken string = "oAuthRevokeRefreshToken"
	OAuthRevokeAllTokens    string = "revoke_all_tokens"
	OauthClientAdded        string = "OauthClientAdded"
	OauthClientRemoved      string = "OauthClientRemoved"
	OauthClientUpdated      string = "OauthClientUpdated"
)

// RPCStorageHandler is a storage manager that uses the redis database.
type RPCStorageHandler struct {
	KeyPrefix        string
	HashKeys         bool
	SuppressRegister bool
	DoReload         func()
	Gw               *Gateway `json:"-"`
}

type RPCDataLoader interface {
	Connect() bool
	GetApiDefinitions(orgId string, tags []string) string
	GetPolicies(orgId string) string
}

// Connect will establish a connection to the RPC
func (r *RPCStorageHandler) Connect() bool {
	slaveOptions := r.Gw.GetConfig().SlaveOptions

	rpcConfig := rpc.Config{
		UseSSL:                slaveOptions.UseSSL,
		SSLInsecureSkipVerify: slaveOptions.SSLInsecureSkipVerify,
		SSLMinVersion:         r.Gw.GetConfig().HttpServerOptions.MinVersion,
		SSLMaxVersion:         r.Gw.GetConfig().HttpServerOptions.MaxVersion,
		ConnectionString:      slaveOptions.ConnectionString,
		RPCKey:                slaveOptions.RPCKey,
		APIKey:                slaveOptions.APIKey,
		GroupID:               slaveOptions.GroupID,
		CallTimeout:           slaveOptions.CallTimeout,
		PingTimeout:           slaveOptions.PingTimeout,
		RPCPoolSize:           slaveOptions.RPCPoolSize,
	}

	return rpc.Connect(
		rpcConfig,
		r.SuppressRegister,
		dispatcherFuncs,
		r.getGroupLoginCallback(r.Gw.GetConfig().SlaveOptions.SynchroniserEnabled),
		func() {
			r.Gw.reloadURLStructure(nil)
		},
		r.DoReload,
	)
}

func (r *RPCStorageHandler) buildNodeInfo() []byte {
	config := r.Gw.GetConfig()
	checkDuration := config.LivenessCheck.CheckDuration
	var intCheckDuration int64 = 10
	if checkDuration != 0 {
		// NodeData.TTL expects an int64 value, so we're getting the number of seconds expressed in int64 instead of time.Second
		intCheckDuration = int64(checkDuration / time.Second)
	}

	r.Gw.getHostDetails()
	node := model.NodeData{
		NodeID:          r.Gw.GetNodeID(),
		GroupID:         config.SlaveOptions.GroupID,
		APIKey:          config.SlaveOptions.APIKey,
		NodeVersion:     VERSION,
		TTL:             intCheckDuration,
		NodeIsSegmented: config.DBAppConfOptions.NodeIsSegmented,
		Tags:            config.DBAppConfOptions.Tags,
		Health:          r.Gw.getHealthCheckInfo(),
		Stats: model.GWStats{
			APIsCount:     r.Gw.apisByIDLen(),
			PoliciesCount: r.Gw.PolicyCount(),
		},
		HostDetails: model.HostDetails{
			Hostname: r.Gw.hostDetails.Hostname,
			PID:      r.Gw.hostDetails.PID,
			Address:  r.Gw.hostDetails.Address,
		},
	}

	data, err := json.Marshal(node)
	if err != nil {
		log.Error("Error marshalling node info", err)
		return nil
	}

	return data
}

func (r *RPCStorageHandler) Disconnect() error {
	request := model.GroupLoginRequest{
		UserKey: r.Gw.GetConfig().SlaveOptions.APIKey,
		GroupID: r.Gw.GetConfig().SlaveOptions.GroupID,
		Node:    r.buildNodeInfo(),
	}

	_, err := rpc.FuncClientSingleton("Disconnect", request)
	return err
}

func (r *RPCStorageHandler) getGroupLoginCallback(synchroniserEnabled bool) func(userKey string, groupID string) interface{} {
	groupLoginCallbackFn := func(userKey string, groupID string) interface{} {
		return model.GroupLoginRequest{
			UserKey: userKey,
			GroupID: groupID,
			Node:    r.buildNodeInfo(),
		}
	}
	if synchroniserEnabled {
		forcer := rpc.NewSyncForcer(r.Gw.StorageConnectionHandler, r.buildNodeInfo)
		groupLoginCallbackFn = forcer.GroupLoginCallback
	}
	return groupLoginCallbackFn
}

func (r *RPCStorageHandler) hashKey(in string) string {
	if !r.HashKeys {
		// Not hashing? Return the raw key
		return in
	}
	return storage.HashStr(in)
}

func (r *RPCStorageHandler) fixKey(keyName string) string {
	setKeyName := r.KeyPrefix + r.hashKey(keyName)

	log.Debug("Input key was: ", r.Gw.obfuscateKey(setKeyName))

	return setKeyName
}

func (r *RPCStorageHandler) cleanKey(keyName string) string {
	setKeyName := strings.Replace(keyName, r.KeyPrefix, "", 1)
	return setKeyName
}

// GetKey will retrieve a key from the database
func (r *RPCStorageHandler) GetKey(keyName string) (string, error) {
	start := time.Now() // get current time
	//	log.Debug("[STORE] Getting WAS: ", keyName)
	//  log.Debug("[STORE] Getting: ", r.fixKey(keyName))
	value, err := r.GetRawKey(r.fixKey(keyName))

	elapsed := time.Since(start)
	log.Debug("GetKey took ", elapsed)

	return value, err
}

func (r *RPCStorageHandler) GetRawKey(keyName string) (string, error) {
	cacheEnabled := r.Gw.GetConfig().SlaveOptions.EnableRPCCache

	var cacheStore cache.Repository
	if cacheEnabled {
		cacheStore = r.Gw.RPCGlobalCache
		if strings.Contains(keyName, "cert-") {
			cacheStore = r.Gw.RPCCertCache
		}

		if cachedVal, found := cacheStore.Get(keyName); found {
			switch typedVal := cachedVal.(type) {
			case string:
				return typedVal, nil
			case error:
				return "", typedVal
			}
		}
	}

	if rpc.IsEmergencyMode() {
		return "", storage.ErrMDCBConnectionLost
	}

	value, err := rpc.FuncClientSingleton("GetKey", keyName)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"GetKey",
			err,
			map[string]string{"keyName": keyName},
		)
		if r.IsRetriableError(err) && rpc.Login() {
			return r.GetRawKey(keyName)
		}
		if cacheEnabled {
			// Errors, and key not found, should be cached for a small amount of time
			cacheStore.Set(keyName, storage.ErrKeyNotFound, 1)
		}
		return "", storage.ErrKeyNotFound
	}

	if cacheEnabled {
		cacheStore.Set(keyName, value, cache.DefaultExpiration)
	}

	return value.(string), nil
}

func (r *RPCStorageHandler) GetMultiKey(keyNames []string) ([]string, error) {
	var err error
	var value string

	for _, key := range keyNames {
		value, err = r.GetKey(key)
		if err == nil {
			return []string{value}, nil
		}
	}

	return nil, err
}

func (r *RPCStorageHandler) GetExp(keyName string) (int64, error) {
	log.Debug("GetExp called")
	value, err := rpc.FuncClientSingleton("GetExp", r.fixKey(keyName))
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"GetExp",
			err,
			map[string]string{
				"keyName":      keyName,
				"fixedKeyName": r.fixKey(keyName),
			},
		)
		if r.IsRetriableError(err) {
			if rpc.Login() {
				return r.GetExp(keyName)
			}
		}
		log.Error("Error trying to get TTL: ", err)
		return 0, storage.ErrKeyNotFound
	}
	return value.(int64), nil
}

func (r *RPCStorageHandler) SetExp(keyName string, timeout int64) error {
	log.Error("RPCStorageHandler.SetExp - Not Implemented")
	return nil
}

// SetKey will create (or update) a key value in the store
func (r *RPCStorageHandler) SetKey(keyName, session string, timeout int64) error {
	start := time.Now() // get current time
	ibd := model.InboundData{
		KeyName:      r.fixKey(keyName),
		SessionState: session,
		Timeout:      timeout,
	}

	_, err := rpc.FuncClientSingleton("SetKey", ibd)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"SetKey",
			err,
			map[string]string{
				"keyName":      keyName,
				"fixedKeyName": ibd.KeyName,
			},
		)

		if r.IsRetriableError(err) {
			if rpc.Login() {
				return r.SetKey(keyName, session, timeout)
			}
		}

		log.Debug("Error trying to set value:", err)
		return err
	}

	elapsed := time.Since(start)
	log.Debug("SetKey took ", elapsed)
	return nil

}

func (r *RPCStorageHandler) SetRawKey(keyName, session string, timeout int64) error {
	return nil
}

// Decrement will decrement a key in redis
func (r *RPCStorageHandler) Decrement(keyName string) {
	log.Warning("Decrement called")
	_, err := rpc.FuncClientSingleton("Decrement", keyName)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"Decrement",
			err,
			map[string]string{
				"keyName": keyName,
			},
		)
	}
	if r.IsRetriableError(err) {
		if rpc.Login() {
			r.Decrement(keyName)
			return
		}
	}
}

// IncrementWithExpire will increment a key in redis
func (r *RPCStorageHandler) IncrememntWithExpire(keyName string, expire int64) int64 {

	ibd := model.InboundData{
		KeyName: keyName,
		Expire:  expire,
	}

	val, err := rpc.FuncClientSingleton("IncrememntWithExpire", ibd)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"IncrememntWithExpire",
			err,
			map[string]string{
				"keyName": keyName,
			},
		)
	}
	if r.IsRetriableError(err) {
		if rpc.Login() {
			return r.IncrememntWithExpire(keyName, expire)
		}
	}

	if val == nil {
		log.Warning("RPC increment returned nil value, returning 0")
		return 0
	}

	return val.(int64)

}

// GetKeys will return all keys according to the filter (filter is a prefix - e.g. tyk.keys.*)
func (r *RPCStorageHandler) GetKeys(filter string) []string {
	log.Error("RPCStorageHandler.GetKeys - Not Implemented")
	return nil
}

// GetKeysAndValuesWithFilter will return all keys and their values with a filter
func (r *RPCStorageHandler) GetKeysAndValuesWithFilter(filter string) map[string]string {

	searchStr := r.KeyPrefix + r.hashKey(filter) + "*"
	log.Debug("[STORE] Getting list by: ", searchStr)

	kvPair, err := rpc.FuncClientSingleton("GetKeysAndValuesWithFilter", searchStr)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"GetKeysAndValuesWithFilter",
			err,
			map[string]string{
				"searchStr": searchStr,
			},
		)

		if r.IsRetriableError(err) {
			if rpc.Login() {
				return r.GetKeysAndValuesWithFilter(filter)
			}
		}

		return nil
	}

	returnValues := make(map[string]string)

	for i, v := range kvPair.(*model.KeysValuesPair).Keys {
		returnValues[r.cleanKey(v)] = kvPair.(*model.KeysValuesPair).Values[i]
	}

	return returnValues
}

// GetKeysAndValues will return all keys and their values - not to be used lightly
func (r *RPCStorageHandler) GetKeysAndValues() map[string]string {

	searchStr := r.KeyPrefix + "*"

	kvPair, err := rpc.FuncClientSingleton("GetKeysAndValues", searchStr)
	if err != nil {
		rpc.EmitErrorEvent(rpc.FuncClientSingletonCall, "GetKeysAndValues", err)

		if r.IsRetriableError(err) {
			if rpc.Login() {
				return r.GetKeysAndValues()
			}
		}

		return nil
	}

	returnValues := make(map[string]string)
	for i, v := range kvPair.(*model.KeysValuesPair).Keys {
		returnValues[r.cleanKey(v)] = kvPair.(*model.KeysValuesPair).Values[i]
	}

	return returnValues

}

// DeleteKey will remove a key from the database
func (r *RPCStorageHandler) DeleteKey(keyName string) bool {

	log.Debug("DEL Key was: ", r.Gw.obfuscateKey(keyName))
	log.Debug("DEL Key became: ", r.Gw.obfuscateKey(r.fixKey(keyName)))
	ok, err := rpc.FuncClientSingleton("DeleteKey", r.fixKey(keyName))
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"DeleteKey",
			err,
			map[string]string{
				"keyName":      keyName,
				"fixedKeyName": r.fixKey(keyName),
			},
		)

		if r.IsRetriableError(err) {
			if rpc.Login() {
				return r.DeleteKey(keyName)
			}
		}
	}

	return ok == true
}

func (r *RPCStorageHandler) DeleteRawKeys(keys []string) bool {
	ret, err := rpc.FuncClientSingleton("DeleteRawKeys", keys)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"DeleteKey",
			err,
			nil,
		)

		if r.IsRetriableError(err) {
			if rpc.Login() {
				return r.DeleteRawKeys(keys)
			}
		}
	}
	success, ok := ret.(bool)
	return success && ok
}

func (r *RPCStorageHandler) DeleteAllKeys() bool {
	log.Warning("Not implementated")
	return false
}

// DeleteKey will remove a key from the database without prefixing, assumes user knows what they are doing
func (r *RPCStorageHandler) DeleteRawKey(keyName string) bool {
	ok, err := rpc.FuncClientSingleton("DeleteRawKey", keyName)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"DeleteRawKey",
			err,
			map[string]string{
				"keyName": keyName,
			},
		)

		if r.IsRetriableError(err) {
			if rpc.Login() {
				return r.DeleteRawKey(keyName)
			}
		}
	}

	return ok == true
}

// DeleteKeys will remove a group of keys in bulk
func (r *RPCStorageHandler) DeleteKeys(keys []string) bool {
	if len(keys) > 0 {
		asInterface := make([]string, len(keys))
		for i, v := range keys {
			asInterface[i] = r.fixKey(v)
		}

		log.Debug("Deleting: ", asInterface)
		ok, err := rpc.FuncClientSingleton("DeleteKeys", asInterface)
		if err != nil {
			rpc.EmitErrorEventKv(
				rpc.FuncClientSingletonCall,
				"DeleteKeys",
				err,
				map[string]string{
					"keys":        strings.Join(keys, ","),
					"asInterface": strings.Join(asInterface, ","),
				},
			)

			if r.IsRetriableError(err) {
				if rpc.Login() {
					return r.DeleteKeys(keys)
				}
			}
		}

		return ok == true
	}
	log.Debug("RPCStorageHandler called DEL - Nothing to delete")
	return true
}

// StartPubSubHandler will listen for a signal and run the callback with the message
func (r *RPCStorageHandler) StartPubSubHandler(_ string, _ func(*temporalmodel.Message)) error {
	log.Warning("RPCStorageHandler.StartPubSubHandler - NO PUBSUB DEFINED")
	return nil
}

func (r *RPCStorageHandler) Publish(channel, message string) error {
	log.Warning("RPCStorageHandler.Publish - NO PUBSUB DEFINED")
	return nil
}

func (r *RPCStorageHandler) GetAndDeleteSet(keyName string) []interface{} {
	log.Error("RPCStorageHandler.GetAndDeleteSet - Not implemented, please disable your purger")
	return nil
}

func (r *RPCStorageHandler) AppendToSet(keyName, value string) {
	ibd := model.InboundData{
		KeyName: keyName,
		Value:   value,
	}

	_, err := rpc.FuncClientSingleton("AppendToSet", ibd)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"AppendToSet",
			err,
			map[string]string{
				"keyName": keyName,
			},
		)
	}
	if r.IsRetriableError(err) {
		if rpc.Login() {
			r.AppendToSet(keyName, value)
		}
	}
}

// SetScrollingWindow is used in the rate limiter to handle rate limits fairly.
func (r *RPCStorageHandler) SetRollingWindow(keyName string, per int64, val string, pipeline bool) (int, []interface{}) {
	start := time.Now() // get current time
	ibd := model.InboundData{
		KeyName: keyName,
		Per:     per,
		Expire:  -1,
	}

	intVal, err := rpc.FuncClientSingleton("SetRollingWindow", ibd)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"SetRollingWindow",
			err,
			map[string]string{
				"keyName": keyName,
				"per":     strconv.Itoa(int(per)),
			},
		)

		if r.IsRetriableError(err) {
			if rpc.Login() {
				return r.SetRollingWindow(keyName, per, val, false)
			}
		}
	}

	elapsed := time.Since(start)
	log.Debug("SetRollingWindow took ", elapsed)

	if intVal == nil {
		log.Warning("RPC Handler: SetRollingWindow() returned nil, returning 0")
		return 0, nil
	}

	return intVal.(int), nil

}

func (r *RPCStorageHandler) GetRollingWindow(keyName string, per int64, pipeline bool) (int, []interface{}) {
	log.Warning("Not Implemented!")
	return 0, nil
}

func (r RPCStorageHandler) GetSet(keyName string) (map[string]string, error) {
	log.Error("RPCStorageHandler.GetSet - Not implemented")
	return nil, nil
}

func (r RPCStorageHandler) AddToSet(keyName, value string) {
	log.Error("RPCStorageHandler.AddToSet - Not implemented")
}

func (r RPCStorageHandler) RemoveFromSet(keyName, value string) {
	log.Error("RPCStorageHandler.RemoveFromSet - Not implemented")
}

func (r RPCStorageHandler) IsRetriableError(err error) bool {
	if err != nil {
		errMsg := err.Error()
		// Access denied errors (authentication issues)
		if errMsg == "Access Denied" {
			return true
		}
		// Timeout errors from gorpc library
		if strings.Contains(errMsg, "Cannot obtain response during timeout") {
			return true
		}
	}
	return false
}

// GetAPIDefinitions will pull API definitions from the RPC server
func (r *RPCStorageHandler) GetApiDefinitions(orgId string, tags []string) string {
	dr := model.DefRequest{
		OrgId:   orgId,
		Tags:    tags,
		LoadOAS: true,
	}

	defString, err := rpc.FuncClientSingleton("GetApiDefinitions", dr)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"GetApiDefinitions",
			err,
			map[string]string{
				"orgId": orgId,
				"tags":  strings.Join(tags, ","),
			},
		)

		if r.IsRetriableError(err) {
			if rpc.Login() {
				return r.GetApiDefinitions(orgId, tags)
			}
		}

		return ""
	}
	log.Debug("API Definitions retrieved")

	if defString == nil {
		log.Warning("RPC Handler: GetApiDefinitions() returned nil, returning empty string")
		return ""
	}
	return defString.(string)
}

// GetPolicies will pull Policies from the RPC server
func (r *RPCStorageHandler) GetPolicies(orgId string) string {
	defString, err := rpc.FuncClientSingleton("GetPolicies", orgId)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"GetPolicies",
			err,
			map[string]string{
				"orgId": orgId,
			},
		)

		if r.IsRetriableError(err) {
			if rpc.Login() {
				return r.GetPolicies(orgId)
			}
		}

		return ""
	}

	if defString != nil {
		return defString.(string)
	}
	return ""
}

// CheckForReload will start a long poll
func (r *RPCStorageHandler) CheckForReload(orgId string) bool {
	select {
	case <-r.Gw.ctx.Done():
		return false
	default:
	}

	log.Debug("[RPC STORE] Check Reload called...")
	reload, err := rpc.FuncClientSingleton("CheckReload", orgId)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			"CheckReload",
			err,
			map[string]string{
				"orgId": orgId,
			},
		)
		if r.IsRetriableError(err) {
			log.Warning("[RPC STORE] CheckReload: Not logged in")
			if rpc.Login() {
				r.CheckForReload(orgId)
			}
		} else if !strings.Contains(err.Error(), "Cannot obtain response during") {
			forcer := rpc.NewSyncForcer(r.Gw.StorageConnectionHandler, r.buildNodeInfo)
			forcer.SetFirstConnection(true)
			log.Warning("[RPC STORE] RPC Reload Checker encountered unexpected error: ", err)
		}

		time.Sleep(1 * time.Second)
	} else if reload == true {
		// Do the reload!
		log.Warning("[RPC STORE] Received Reload instruction!")
		go func() {
			r.Gw.MainNotifier.Notify(Notification{Command: NoticeGroupReload, Gw: r.Gw})
		}()
	}
	return true
}

func (r *RPCStorageHandler) StartRPCLoopCheck(orgId string) {
	if r.Gw.GetConfig().SlaveOptions.DisableKeySpaceSync {
		return
	}

	log.Info("[RPC] Starting keyspace poller")

	for {
		seconds := r.Gw.GetConfig().SlaveOptions.KeySpaceSyncInterval
		r.CheckForKeyspaceChanges(orgId)
		time.Sleep(time.Duration(seconds) * time.Second)
	}
}

func (r *RPCStorageHandler) StartRPCKeepaliveWatcher() {
	log.WithFields(logrus.Fields{
		"prefix": "RPC Conn Mgr",
	}).Info("[RPC Conn Mgr] Starting keepalive watcher...")
	for {

		select {
		case <-r.Gw.ctx.Done():
			return
		default:
		}

		if err := r.SetKey("0000", "0000", 10); err != nil {
			log.WithError(err).WithFields(logrus.Fields{
				"prefix": "RPC Conn Mgr",
			}).Warning("Can't connect to RPC layer")

			if r.IsRetriableError(err) {
				if rpc.Login() {
					continue
				}
			}

			if strings.Contains(err.Error(), "Cannot obtain response during timeout") {
				continue
			}
		}

		time.Sleep(10 * time.Second)
	}
}

// CheckForKeyspaceChanges will poll for keysace changes
func (r *RPCStorageHandler) CheckForKeyspaceChanges(orgId string) {
	log.Debug("Checking for keyspace changes...")

	var keys interface{}
	var err error
	var funcName string
	var req interface{}

	reqData := map[string]string{}
	if groupID := r.Gw.GetConfig().SlaveOptions.GroupID; groupID == "" {
		funcName = "GetKeySpaceUpdate"
		req = orgId
		reqData["orgId"] = orgId
	} else {
		funcName = "GetGroupKeySpaceUpdate"
		req = model.GroupKeySpaceRequest{
			OrgID:   orgId,
			GroupID: groupID,
		}
		reqData["orgId"] = orgId
		reqData["GroupID"] = groupID
	}

	keys, err = rpc.FuncClientSingleton(funcName, req)
	if err != nil {
		rpc.EmitErrorEventKv(
			rpc.FuncClientSingletonCall,
			funcName,
			err,
			reqData,
		)
		if r.IsRetriableError(err) {
			if rpc.Login() {
				r.CheckForKeyspaceChanges(orgId)
			}
		}
		log.Warning("Keyspace warning: ", err)
		return
	}

	if keys == nil {
		log.Info("Keys returned nil object, skipping check")
		return
	}

	if len(keys.([]string)) > 0 {
		log.Info("Keyspace changes detected, updating local cache")
		go r.ProcessKeySpaceChanges(keys.([]string), orgId)
	}
}

func (gw *Gateway) getSessionAndCreate(keyName string, r *RPCStorageHandler, isHashed bool, orgId string) {

	key := keyName
	// avoid double hashing
	if !isHashed {
		key = storage.HashKey(keyName, gw.GetConfig().HashKeys)
	}

	sessionString, err := r.GetRawKey("apikey-" + key)
	if err != nil {
		log.Error("Key not found in master - skipping")
	} else {
		gw.handleAddKey(key, sessionString, orgId)
	}
}

func (gw *Gateway) ProcessSingleOauthClientEvent(apiId, oauthClientId, orgID, event string) {
	store, _, err := gw.GetStorageForApi(apiId)
	if err != nil {
		log.Error("Could not get oauth storage for api")
		return
	}

	switch event {
	case OauthClientAdded:
		// on add: pull from rpc and save it in local redis
		client, err := store.GetClient(oauthClientId)
		if err != nil {
			log.WithError(err).Error("Could not retrieve new oauth client information")
			return
		}

		err = store.SetClient(oauthClientId, orgID, client, false)
		if err != nil {
			log.WithError(err).Error("Could not save oauth client.")
			return
		}

		log.Info("oauth client created successfully")
	case OauthClientRemoved:
		// on remove: remove from local redis
		err := store.DeleteClient(oauthClientId, orgID, false)
		if err != nil {
			log.Errorf("Could not delete oauth client with id: %v", oauthClientId)
			return
		}
		log.Infof("Oauth Client deleted successfully")
	case OauthClientUpdated:
		// on update: delete from local redis and pull again from rpc
		_, err := store.GetClient(oauthClientId)
		if err != nil {
			log.WithError(err).Error("Could not retrieve oauth client information")
			return
		}

		err = store.DeleteClient(oauthClientId, orgID, false)
		if err != nil {
			log.WithError(err).Error("Could not delete oauth client")
			return
		}

		client, err := store.GetClient(oauthClientId)
		if err != nil {
			log.WithError(err).Error("Could not retrieve oauth client information")
			return
		}

		err = store.SetClient(oauthClientId, orgID, client, false)
		if err != nil {
			log.WithError(err).Error("Could not save oauth client.")
			return
		}
		log.Info("oauth client updated successfully")
	default:
		log.Warningf("Oauth client event not supported:%v", event)
	}
}

// ProcessOauthClientsOps performs the appropriate action for the received clients
// it can be any of the Create,Update and Delete operations
func (gw *Gateway) ProcessOauthClientsOps(clients map[string]string) {
	for clientInfo, action := range clients {
		// clientInfo is: APIID.ClientID.OrgID
		eventValues := strings.Split(clientInfo, ".")
		apiId := eventValues[0]
		oauthClientId := eventValues[1]
		orgID := eventValues[2]

		gw.ProcessSingleOauthClientEvent(apiId, oauthClientId, orgID, action)
	}
}

// ProcessKeySpaceChanges receives an array of keys to be processed, those keys are considered changes in the keyspace in the
// management layer, they could be: regular keys (hashed, unhashed), revoke oauth client, revoke single oauth token,
// certificates (added, removed), oauth client (added, updated, removed), user key events (reset)
func (r *RPCStorageHandler) ProcessKeySpaceChanges(keys []string, orgId string) {
	keysToReset := map[string]bool{}
	TokensToBeRevoked := map[string]string{}
	ClientsToBeRevoked := map[string]string{}
	notRegularKeys := map[string]bool{}
	CertificatesToRemove := map[string]string{}
	CertificatesToAdd := map[string]string{}
	OauthClients := map[string]string{}
	apiIDsToDeleteCache := make([]string, 0)
	userKeyResets := make(map[string]string)

	for _, key := range keys {
		splitKeys := strings.Split(key, ":")
		if len(splitKeys) > 1 {
			action := splitKeys[len(splitKeys)-1]
			switch action {
			case ResetQuota:
				keysToReset[splitKeys[0]] = true
			case CertificateRemoved:
				CertificatesToRemove[key] = splitKeys[0]
				notRegularKeys[key] = true
			case CertificateAdded:
				CertificatesToAdd[key] = splitKeys[0]
				notRegularKeys[key] = true
			case OAuthRevokeToken, OAuthRevokeAccessToken, OAuthRevokeRefreshToken:
				TokensToBeRevoked[splitKeys[0]] = key
				notRegularKeys[key] = true
			case OAuthRevokeAllTokens:
				ClientsToBeRevoked[splitKeys[1]] = key
				notRegularKeys[key] = true
			case OauthClientAdded, OauthClientUpdated, OauthClientRemoved:
				OauthClients[splitKeys[0]] = action
				notRegularKeys[key] = true
			case NoticeDeleteAPICache.String():
				apiIDsToDeleteCache = append(apiIDsToDeleteCache, splitKeys[0])
				notRegularKeys[key] = true
			case NoticeUserKeyReset.String():
				keyParts := strings.Split(splitKeys[0], ".")
				if len(keyParts) != 2 {
					log.Error("Invalid user key reset format")
					continue
				}
				userKeyResets[keyParts[0]] = keyParts[1]
			default:
				log.Debug("ignoring processing of action:", action)
			}
		}
	}
	for oldKey, newKey := range userKeyResets {
		if r.Gw.GetConfig().SlaveOptions.APIKey == oldKey {
			config := r.Gw.GetConfig()

			// Updating the key in the KV store if we are using one
			r.Gw.updateKeyInStore(config.Private.EdgeOriginalAPIKeyPath, newKey)

			config.SlaveOptions.APIKey = newKey
			r.Gw.SetConfig(config)
			connected := r.Connect()
			if !connected {
				log.Error("Failed to reconnect to RPC storage")
				continue
			}
		}
		ok := r.Gw.MainNotifier.Notify(Notification{
			Command: NoticeUserKeyReset,
			Payload: fmt.Sprintf("%s.%s:%s", oldKey, newKey, NoticeUserKeyReset),
			Gw:      r.Gw,
		})
		if !ok {
			log.Error("Failed to notify other gateways about user key reset")
		}
	}
	// Process OAuth clients
	r.Gw.ProcessOauthClientsOps(OauthClients)

	for clientId, key := range ClientsToBeRevoked {
		splitKeys := strings.Split(key, ":")
		apiId := splitKeys[0]
		clientSecret := splitKeys[2]
		storage, _, err := r.Gw.GetStorageForApi(apiId)
		if err != nil {
			continue
		}
		_, tokens, _ := RevokeAllTokens(storage, clientId, clientSecret)
		keys = append(keys, tokens...)
	}

	//single and specific tokens
	for token, key := range TokensToBeRevoked {
		//key formed as: token:apiId:tokenActionTypeHint
		//but hashed as: token#hashed:apiId:tokenActionTypeHint
		splitKeys := strings.Split(key, ":")
		apiId := splitKeys[1]
		tokenActionTypeHint := splitKeys[2]
		hashedKey := strings.Contains(token, "#hashed")
		if !hashedKey {
			storage, _, err := r.Gw.GetStorageForApi(apiId)
			if err != nil {
				continue
			}
			var tokenTypeHint string
			switch tokenActionTypeHint {
			case OAuthRevokeAccessToken:
				tokenTypeHint = "access_token"
			case OAuthRevokeRefreshToken:
				tokenTypeHint = "refresh_token"
			}
			RevokeToken(storage, token, tokenTypeHint)
		} else {
			token = strings.Split(token, "#")[0]
			r.Gw.handleDeleteHashedKey(token, orgId, apiId, false)
		}
		r.Gw.SessionCache.Delete(token)
		r.Gw.RPCGlobalCache.Delete(r.KeyPrefix + token)
	}

	// remove certs
	for _, certId := range CertificatesToRemove {
		log.Debugf("Removing certificate: %v", certId)
		r.Gw.CertificateManager.Delete(certId, orgId)
		r.Gw.RPCCertCache.Delete("cert-raw-" + certId)
	}

	for _, certId := range CertificatesToAdd {
		log.Debugf("Adding certificate: %v", certId)
		//If we are in a slave node, MDCB Storage GetRaw should get the certificate from MDCB and cache it locally
		content, err := r.Gw.CertificateManager.GetRaw(certId)
		if content == "" && err != nil {
			log.Debugf("Error getting certificate content")
		}
	}

	synchronizerEnabled := r.Gw.GetConfig().SlaveOptions.SynchroniserEnabled
	for _, key := range keys {
		// Skip keys that are user keys to be reset
		splitKeys := strings.Split(key, ":")
		if len(splitKeys) > 1 {
			userKeys := strings.Split(splitKeys[0], ".")
			if len(userKeys) == 2 {
				_, ok := userKeyResets[userKeys[0]]
				if ok {
					continue
				}
			}
		}
		_, isOauthTokenKey := notRegularKeys[key]
		if !isOauthTokenKey {
			splitKeys := strings.Split(key, ":")
			_, resetQuota := keysToReset[splitKeys[0]]

			isHashed := len(splitKeys) > 1 && splitKeys[1] == "hashed"
			var status int
			var err error
			if isHashed {
				log.Info("--> removing cached (hashed) key: ", splitKeys[0])
				key = splitKeys[0]
				_, status = r.Gw.handleDeleteHashedKey(key, orgId, "", resetQuota)
			} else {
				log.Info("--> removing cached key: ", r.Gw.obfuscateKey(key))
				// in case it's a username (basic auth) or custom-key then generate the token
				if storage.TokenOrg(key) == "" {
					key = r.Gw.generateToken(orgId, key)
				}
				_, status = r.Gw.handleDeleteKey(key, orgId, "-1", resetQuota)
				// check if we must remove the key by custom key id
				status, err = r.deleteUsingTokenID(key, orgId, resetQuota, status)
				if err != nil {
					log.Debugf("cannot remove key:%v status: %v", key, status)
				}
			}

			// if key not found locally and synchroniser disabled then we should not pull it from management layer
			if status == http.StatusNotFound && !synchronizerEnabled {
				continue
			}
			r.Gw.getSessionAndCreate(key, r, isHashed, orgId)
			r.Gw.SessionCache.Delete(key)
			r.Gw.RPCGlobalCache.Delete(r.KeyPrefix + key)
		}
	}

	for _, apiID := range apiIDsToDeleteCache {
		if r.Gw.invalidateAPICache(apiID) {
			log.WithField("apiID", apiID).Info("cache invalidated")
			continue
		}

		log.WithField("apiID", apiID).Error("cache invalidation failed")
	}

	// Notify rest of gateways in cluster to flush cache
	n := Notification{
		Command: KeySpaceUpdateNotification,
		Payload: strings.Join(keys, ","),
		Gw:      r.Gw,
	}
	r.Gw.MainNotifier.Notify(n)
}

// Function to handle fallback deletion using token ID
func (r *RPCStorageHandler) deleteUsingTokenID(key, orgId string, resetQuota bool, status int) (int, error) {
	if status == http.StatusNotFound {
		id, err := storage.TokenID(key)
		if err == nil {
			_, status = r.Gw.handleDeleteKey(id, orgId, "-1", resetQuota)
		}
	}
	return status, nil
}

func (r *RPCStorageHandler) DeleteScanMatch(pattern string) bool {
	log.Error("RPCStorageHandler.DeleteScanMatch - Not implemented")
	return false
}

func (r *RPCStorageHandler) GetKeyPrefix() string {
	log.Error("RPCStorageHandler.GetKeyPrefix - Not implemented")
	return ""
}

func (r *RPCStorageHandler) AddToSortedSet(keyName, value string, score float64) {
	r.Gw.handleGlobalAddToSortedSet(keyName, value, score)
}

func (r *RPCStorageHandler) GetSortedSetRange(keyName, scoreFrom, scoreTo string) ([]string, []float64, error) {
	return r.Gw.handleGetSortedSetRange(keyName, scoreFrom, scoreTo)
}

func (r *RPCStorageHandler) RemoveSortedSetRange(keyName, scoreFrom, scoreTo string) error {
	return r.Gw.handleRemoveSortedSetRange(keyName, scoreFrom, scoreTo)
}

func (r *RPCStorageHandler) RemoveFromList(keyName, value string) error {
	log.Error("Not implemented")
	return nil
}

func (r *RPCStorageHandler) GetListRange(keyName string, from, to int64) ([]string, error) {
	log.Error("Not implemented")
	return nil, nil
}

func (r *RPCStorageHandler) Exists(keyName string) (bool, error) {
	log.Error("Not implemented")
	return false, nil
}

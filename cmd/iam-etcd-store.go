/*
 * MinIO Cloud Storage, (C) 2019 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	jwtgo "github.com/dgrijalva/jwt-go"
	"github.com/minio/minio-go/v7/pkg/set"
	"github.com/minio/minio/cmd/logger"
	"github.com/minio/minio/pkg/auth"
	iampolicy "github.com/minio/minio/pkg/iam/policy"
	"github.com/minio/minio/pkg/madmin"
	etcd "go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/mvcc/mvccpb"
)

var defaultContextTimeout = 30 * time.Second

func etcdKvsToSet(prefix string, kvs []*mvccpb.KeyValue) set.StringSet {
	users := set.NewStringSet()
	for _, kv := range kvs {
		user := extractPrefixAndBase(string(kv.Key), prefix) 
		users.Add(user)
	}
	return users
}

func extractPrefixAndBase(s string, prefix string ) string {
		// Extract user by stripping off the `prefix` value as suffix,
		// then strip off the remaining basename to obtain the prefix
		// value, usually in the following form.
		//
		//  key := "config/iam/users/newuser/identity.json"
		//  prefix := "config/iam/users/"
		//  v := trim(trim(key, prefix), base(key)) == "newuser"
		//
		return path.Clean(strings.TrimSuffix(strings.TrimPrefix(string(s), prefix), path.Base(string(s))))
}

func etcdKvsToSetPolicyDB(prefix string, kvs []*mvccpb.KeyValue) set.StringSet {
	items := set.NewStringSet()
	for _, kv := range kvs {
		// Extract user item by stripping off prefix and then
		// stripping of ".json" suffix.
		//
		// key := "config/iam/policydb/users/myuser1.json"
		// prefix := "config/iam/policydb/users/"
		// v := trimSuffix(trimPrefix(key, prefix), ".json")
		key := string(kv.Key)
		item := path.Clean(strings.TrimSuffix(strings.TrimPrefix(key, prefix), ".json"))
		items.Add(item)
	}
	return items
}

// IAMEtcdStore implements IAMStorageAPI
type IAMEtcdStore struct {
	sync.RWMutex

	client *etcd.Client
}

func newIAMEtcdStore() *IAMEtcdStore {
	return &IAMEtcdStore{client: globalEtcdClient}
}

func (ies *IAMEtcdStore) lock() {
	ies.Lock()
}

func (ies *IAMEtcdStore) unlock() {
	ies.Unlock()
}

func (ies *IAMEtcdStore) rlock() {
	ies.RLock()
}

func (ies *IAMEtcdStore) runlock() {
	ies.RUnlock()
}

func (ies *IAMEtcdStore) saveIAMConfig(ctx context.Context, item interface{}, path string, opts ...options) error {
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if globalConfigEncrypted {
		data, err = madmin.EncryptData(globalActiveCred.String(), data)
		if err != nil {
			return err
		}
	}
	return saveKeyEtcd(ctx, ies.client, path, data, opts...)
}


func (ies *IAMEtcdStore) getIAMConfig(item interface{}, value []byte) error {
	conf := value
	var err error
	if globalConfigEncrypted && !utf8.Valid(value) {
		conf, err = madmin.DecryptData(globalActiveCred.String(), bytes.NewReader(conf))
		if err != nil {
			return err
		}
	}
	return json.Unmarshal(conf, item)
}


func (ies *IAMEtcdStore) loadIAMConfig(ctx context.Context, item interface{}, path string) error {
	pdata, err := readKeyEtcd(ctx, ies.client, path)
	if err != nil {
		return err
	}

	if globalConfigEncrypted && !utf8.Valid(pdata) {
		pdata, err = madmin.DecryptData(globalActiveCred.String(), bytes.NewReader(pdata))
		if err != nil {
			return err
		}
	}

	return json.Unmarshal(pdata, item)
}

func (ies *IAMEtcdStore) deleteIAMConfig(ctx context.Context, path string) error {
	return deleteKeyEtcd(ctx, ies.client, path)
}

func (ies *IAMEtcdStore) migrateUsersConfigToV1(ctx context.Context, isSTS bool) error {
	basePrefix := iamConfigUsersPrefix
	if isSTS {
		basePrefix = iamConfigSTSPrefix
	}

	ctx, cancel := context.WithTimeout(ctx, defaultContextTimeout)
	defer cancel()
	r, err := ies.client.Get(ctx, basePrefix, etcd.WithPrefix(), etcd.WithKeysOnly())
	if err != nil {
		return err
	}

	users := etcdKvsToSet(basePrefix, r.Kvs)
	for _, user := range users.ToSlice() {
		{
			// 1. check if there is a policy file in the old loc.
			oldPolicyPath := pathJoin(basePrefix, user, iamPolicyFile)
			var policyName string
			err := ies.loadIAMConfig(ctx, &policyName, oldPolicyPath)
			if err != nil {
				switch err {
				case errConfigNotFound:
					// No mapped policy or already migrated.
				default:
					// corrupt data/read error, etc
				}
				goto next
			}

			// 2. copy policy to new loc.
			mp := newMappedPolicy(policyName)
			userType := regularUser
			if isSTS {
				userType = stsUser
			}
			path := getMappedPolicyPath(user, userType, false)
			if err := ies.saveIAMConfig(ctx, mp, path); err != nil {
				return err
			}

			// 3. delete policy file in old loc.
			deleteKeyEtcd(ctx, ies.client, oldPolicyPath)
		}

	next:
		// 4. check if user identity has old format.
		identityPath := pathJoin(basePrefix, user, iamIdentityFile)
		var cred auth.Credentials
		if err := ies.loadIAMConfig(ctx, &cred, identityPath); err != nil {
			switch err {
			case errConfigNotFound:
				// This case should not happen.
			default:
				// corrupt file or read error
			}
			continue
		}

		// If the file is already in the new format,
		// then the parsed auth.Credentials will have
		// the zero value for the struct.
		var zeroCred auth.Credentials
		if cred.Equal(zeroCred) {
			// nothing to do
			continue
		}

		// Found a id file in old format. Copy value
		// into new format and save it.
		cred.AccessKey = user
		u := newUserIdentity(cred)
		if err := ies.saveIAMConfig(ctx, u, identityPath); err != nil {
			logger.LogIf(ctx, err)
			return err
		}

		// Nothing to delete as identity file location
		// has not changed.
	}
	return nil
}

func (ies *IAMEtcdStore) migrateToV1(ctx context.Context) error {
	logTime("Performing Migration...")
	defer logTime("Migration done")
	var iamFmt iamFormat
	path := getIAMFormatFilePath()
	if err := ies.loadIAMConfig(ctx, &iamFmt, path); err != nil {
		switch err {
		case errConfigNotFound:
			// Need to migrate to V1.
		default:
			return err
		}
	} else {
		if iamFmt.Version >= iamFormatVersion1 {
			// Already migrated to V1 of higher!
			return nil
		}
		// This case should not happen
		// (i.e. Version is 0 or negative.)
		return errors.New("got an invalid IAM format version")

	}

	// Migrate long-term users
	if err := ies.migrateUsersConfigToV1(ctx, false); err != nil {
		logger.LogIf(ctx, err)
		return err
	}
	// Migrate STS users
	if err := ies.migrateUsersConfigToV1(ctx, true); err != nil {
		logger.LogIf(ctx, err)
		return err
	}
	// Save iam version file.
	if err := ies.saveIAMConfig(ctx, newIAMFormatVersion1(), path); err != nil {
		logger.LogIf(ctx, err)
		return err
	}
	return nil
}

// Should be called under config migration lock
func (ies *IAMEtcdStore) migrateBackendFormat(ctx context.Context) error {
	return ies.migrateToV1(ctx)
}

//deprecated
func (ies *IAMEtcdStore) loadPolicyDoc(ctx context.Context, policy string, m map[string]iampolicy.Policy) error {
	var p iampolicy.Policy
	err := ies.loadIAMConfig(ctx, &p, getPolicyDocPath(policy))
	if err != nil {
		if err == errConfigNotFound {
			return errNoSuchPolicy
		}
		return err
	}
	m[policy] = p
	return nil
}

func (ies *IAMEtcdStore) getPolicyDoc(ctx context.Context, kvs *mvccpb.KeyValue, m map[string]iampolicy.Policy) error {
	var p iampolicy.Policy
	err := ies.getIAMConfig(&p, kvs.Value)
	if err != nil {
		if err == errConfigNotFound {
			return errNoSuchPolicy
		}
		return err
	}
	policy := extractPrefixAndBase(string(kvs.Key), iamConfigPoliciesPrefix)
	m[policy] = p
	return nil
}


func (ies *IAMEtcdStore) loadPolicyDocs(ctx context.Context, m map[string]iampolicy.Policy) error {

	ctx, cancel := context.WithTimeout(ctx, defaultContextTimeout)
	defer cancel()
	logTime("   Getting Policies from Etcd")
	r, err := ies.client.Get(ctx, iamConfigPoliciesPrefix, etcd.WithPrefix())
	if err != nil {
		return err
	}

	logTime("   extracting policies")
	// extract each policy.
	for _, kvs := range r.Kvs {
		if err = ies.getPolicyDoc(ctx, kvs, m); err != nil && err != errNoSuchPolicy {
			return err
		}
	}
	return nil
}

func (ies *IAMEtcdStore) loadUser(ctx context.Context, user string, userType IAMUserType, m map[string]auth.Credentials) error {
	var u UserIdentity
	err := ies.loadIAMConfig(ctx, &u, getUserIdentityPath(user, userType))
	if err != nil {
		if err == errConfigNotFound {
			return errNoSuchUser
		}
		return err
	}

	if u.Credentials.IsExpired() {
		// Delete expired identity.
		deleteKeyEtcd(ctx, ies.client, getUserIdentityPath(user, userType))
		deleteKeyEtcd(ctx, ies.client, getMappedPolicyPath(user, userType, false))
		return nil
	}

	// If this is a service account, rotate the session key if we are changing the server creds
	if globalOldCred.IsValid() && u.Credentials.IsServiceAccount() {
		if !globalOldCred.Equal(globalActiveCred) {
			m := jwtgo.MapClaims{}
			stsTokenCallback := func(t *jwtgo.Token) (interface{}, error) {
				return []byte(globalOldCred.SecretKey), nil
			}
			if _, err := jwtgo.ParseWithClaims(u.Credentials.SessionToken, m, stsTokenCallback); err == nil {
				jwt := jwtgo.NewWithClaims(jwtgo.SigningMethodHS512, jwtgo.MapClaims(m))
				if token, err := jwt.SignedString([]byte(globalActiveCred.SecretKey)); err == nil {
					u.Credentials.SessionToken = token
					err := ies.saveIAMConfig(ctx, &u, getUserIdentityPath(user, userType))
					if err != nil {
						return err
					}
				}
			}
		}
	}

	if u.Credentials.AccessKey == "" {
		u.Credentials.AccessKey = user
	}
	m[user] = u.Credentials
	return nil

}


func (ies *IAMEtcdStore) getUser(ctx context.Context, userkv *mvccpb.KeyValue, userType IAMUserType, m map[string]auth.Credentials, basePrefix string) error {
	var u UserIdentity
	user := extractPrefixAndBase(string(userkv.Key), basePrefix)
	err := ies.getIAMConfig(&u, userkv.Value)
	if err != nil {
		if err == errConfigNotFound {
			return errNoSuchUser
		}
		return err
	}
	return ies.setupUser(ctx , user, userType, u, m)
}

func (ies *IAMEtcdStore) setupUser(ctx context.Context, userName string, userType IAMUserType, u UserIdentity , m map[string]auth.Credentials) error {
	if u.Credentials.IsExpired() {
		// Delete expired identity.
		deleteKeyEtcd(ctx, ies.client, getUserIdentityPath(userName, userType))
		deleteKeyEtcd(ctx, ies.client, getMappedPolicyPath(userName, userType, false))
		return nil
	}

	// If this is a service account, rotate the session key if we are changing the server creds
	if globalOldCred.IsValid() && u.Credentials.IsServiceAccount() {
		if !globalOldCred.Equal(globalActiveCred) {
			m := jwtgo.MapClaims{}
			stsTokenCallback := func(t *jwtgo.Token) (interface{}, error) {
				return []byte(globalOldCred.SecretKey), nil
			}
			if _, err := jwtgo.ParseWithClaims(u.Credentials.SessionToken, m, stsTokenCallback); err == nil {
				jwt := jwtgo.NewWithClaims(jwtgo.SigningMethodHS512, jwtgo.MapClaims(m))
				if token, err := jwt.SignedString([]byte(globalActiveCred.SecretKey)); err == nil {
					u.Credentials.SessionToken = token
					err := ies.saveIAMConfig(ctx, &u, getUserIdentityPath(userName, userType))
					if err != nil {
						return err
					}
				}
			}
		}
	}

	if u.Credentials.AccessKey == "" {
		u.Credentials.AccessKey = userName
	}
	m[userName] = u.Credentials
	return nil
}


func (ies *IAMEtcdStore) loadUsers(ctx context.Context, userType IAMUserType, m map[string]auth.Credentials) error {

	var basePrefix string
	switch userType {
	case srvAccUser:
		basePrefix = iamConfigServiceAccountsPrefix
	case stsUser:
		basePrefix = iamConfigSTSPrefix
	default:
		basePrefix = iamConfigUsersPrefix
		//start profiling when this method is called until the end
		// if _, err := os.Stat("cpuprofile-users.prof"); os.IsNotExist(err){
		// 	f, _ := os.Create("cpuprofile-users.prof")
		// 	pprof.StartCPUProfile(f)
		// 	defer pprof.StopCPUProfile()
		// }
	}

	cctx, cancel := context.WithTimeout(ctx, defaultContextTimeout)
	defer cancel()

	r, err := ies.client.Get(cctx, basePrefix, etcd.WithPrefix())
	if err != nil {
		return err
	}

	//extract user from resulting etcd query
	for _, userKv := range r.Kvs {
		if err = ies.getUser(ctx, userKv, userType, m ,basePrefix); err != nil && err != errNoSuchUser {
			return err
		}
	}
	return nil
}

func (ies *IAMEtcdStore) loadGroup(ctx context.Context, group string, m map[string]GroupInfo) error {
	var gi GroupInfo
	err := ies.loadIAMConfig(ctx, &gi, getGroupInfoPath(group))
	if err != nil {
		if err == errConfigNotFound {
			return errNoSuchGroup
		}
		return err
	}
	m[group] = gi
	return nil

}

func (ies *IAMEtcdStore) loadGroups(ctx context.Context, m map[string]GroupInfo) error {
	cctx, cancel := context.WithTimeout(ctx, defaultContextTimeout)
	defer cancel()

	r, err := ies.client.Get(cctx, iamConfigGroupsPrefix, etcd.WithPrefix(), etcd.WithKeysOnly())
	if err != nil {
		return err
	}

	groups := etcdKvsToSet(iamConfigGroupsPrefix, r.Kvs)

	// Reload config for all groups.
	for _, group := range groups.ToSlice() {
		if err = ies.loadGroup(ctx, group, m); err != nil && err != errNoSuchGroup {
			return err
		}
	}
	return nil

}

func (ies *IAMEtcdStore) loadMappedPolicy(ctx context.Context, name string, userType IAMUserType, isGroup bool, m map[string]MappedPolicy) error {
	var p MappedPolicy
	err := ies.loadIAMConfig(ctx, &p, getMappedPolicyPath(name, userType, isGroup))
	if err != nil {
		if err == errConfigNotFound {
			return errNoSuchPolicy
		}
		return err
	}
	m[name] = p
	return nil

}

func (ies *IAMEtcdStore) loadMappedPolicies(ctx context.Context, userType IAMUserType, isGroup bool, m map[string]MappedPolicy) error {
	cctx, cancel := context.WithTimeout(ctx, defaultContextTimeout)
	defer cancel()
	var basePrefix string
	if isGroup {
		basePrefix = iamConfigPolicyDBGroupsPrefix
	} else {
		switch userType {
		case srvAccUser:
			basePrefix = iamConfigPolicyDBServiceAccountsPrefix
		case stsUser:
			basePrefix = iamConfigPolicyDBSTSUsersPrefix
		default:
			basePrefix = iamConfigPolicyDBUsersPrefix
		}
	}

	r, err := ies.client.Get(cctx, basePrefix, etcd.WithPrefix(), etcd.WithKeysOnly())
	if err != nil {
		return err
	}

	users := etcdKvsToSetPolicyDB(basePrefix, r.Kvs)

	// Reload config and policies for all users.
	for _, user := range users.ToSlice() {
		if err = ies.loadMappedPolicy(ctx, user, userType, isGroup, m); err != nil && err != errNoSuchPolicy {
			return err
		}
	}
	return nil

}

func (ies *IAMEtcdStore) loadAll(ctx context.Context, sys *IAMSys) error {
	return sys.Load(ctx, ies)
}

func (ies *IAMEtcdStore) savePolicyDoc(ctx context.Context, policyName string, p iampolicy.Policy) error {
	return ies.saveIAMConfig(ctx, &p, getPolicyDocPath(policyName))
}

func (ies *IAMEtcdStore) saveMappedPolicy(ctx context.Context, name string, userType IAMUserType, isGroup bool, mp MappedPolicy, opts ...options) error {
	return ies.saveIAMConfig(ctx, mp, getMappedPolicyPath(name, userType, isGroup), opts...)
}

func (ies *IAMEtcdStore) saveUserIdentity(ctx context.Context, name string, userType IAMUserType, u UserIdentity, opts ...options) error {
	return ies.saveIAMConfig(ctx, u, getUserIdentityPath(name, userType), opts...)
}

func (ies *IAMEtcdStore) saveGroupInfo(ctx context.Context, name string, gi GroupInfo) error {
	return ies.saveIAMConfig(ctx, gi, getGroupInfoPath(name))
}

func (ies *IAMEtcdStore) deletePolicyDoc(ctx context.Context, name string) error {
	err := ies.deleteIAMConfig(ctx, getPolicyDocPath(name))
	if err == errConfigNotFound {
		err = errNoSuchPolicy
	}
	return err
}

func (ies *IAMEtcdStore) deleteMappedPolicy(ctx context.Context, name string, userType IAMUserType, isGroup bool) error {
	err := ies.deleteIAMConfig(ctx, getMappedPolicyPath(name, userType, isGroup))
	if err == errConfigNotFound {
		err = errNoSuchPolicy
	}
	return err
}

func (ies *IAMEtcdStore) deleteUserIdentity(ctx context.Context, name string, userType IAMUserType) error {
	err := ies.deleteIAMConfig(ctx, getUserIdentityPath(name, userType))
	if err == errConfigNotFound {
		err = errNoSuchUser
	}
	return err
}

func (ies *IAMEtcdStore) deleteGroupInfo(ctx context.Context, name string) error {
	err := ies.deleteIAMConfig(ctx, getGroupInfoPath(name))
	if err == errConfigNotFound {
		err = errNoSuchGroup
	}
	return err
}

func (ies *IAMEtcdStore) watch(ctx context.Context, sys *IAMSys) {
	for {
	outerLoop:
		// Refresh IAMSys with etcd watch.
		watchCh := ies.client.Watch(ctx,
			iamConfigPrefix, etcd.WithPrefix(), etcd.WithKeysOnly())

		for {
			select {
			case <-ctx.Done():
				return
			case watchResp, ok := <-watchCh:
				if !ok {
					time.Sleep(1 * time.Second)
					// Upon an error on watch channel
					// re-init the watch channel.
					goto outerLoop
				}
				if err := watchResp.Err(); err != nil {
					logger.LogIf(ctx, err)
					// log and retry.
					time.Sleep(1 * time.Second)
					// Upon an error on watch channel
					// re-init the watch channel.
					goto outerLoop
				}
				for _, event := range watchResp.Events {
					ies.lock()
					ies.reloadFromEvent(sys, event)
					ies.unlock()
				}
			}
		}
	}
}

// sys.RLock is held by caller.
func (ies *IAMEtcdStore) reloadFromEvent(sys *IAMSys, event *etcd.Event) {
	eventCreate := event.IsModify() || event.IsCreate()
	eventDelete := event.Type == etcd.EventTypeDelete
	usersPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigUsersPrefix)
	groupsPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigGroupsPrefix)
	stsPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigSTSPrefix)
	svcPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigServiceAccountsPrefix)
	policyPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigPoliciesPrefix)
	policyDBUsersPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigPolicyDBUsersPrefix)
	policyDBSTSUsersPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigPolicyDBSTSUsersPrefix)
	policyDBGroupsPrefix := strings.HasPrefix(string(event.Kv.Key), iamConfigPolicyDBGroupsPrefix)

	ctx, cancel := context.WithTimeout(context.Background(), defaultContextTimeout)
	defer cancel()

	switch {
	case eventCreate:
		switch {
		case usersPrefix:
			accessKey := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigUsersPrefix))
			ies.loadUser(ctx, accessKey, regularUser, sys.iamUsersMap)
		case stsPrefix:
			accessKey := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigSTSPrefix))
			ies.loadUser(ctx, accessKey, stsUser, sys.iamUsersMap)
		case svcPrefix:
			accessKey := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigServiceAccountsPrefix))
			ies.loadUser(ctx, accessKey, srvAccUser, sys.iamUsersMap)
		case groupsPrefix:
			group := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigGroupsPrefix))
			ies.loadGroup(ctx, group, sys.iamGroupsMap)
			gi := sys.iamGroupsMap[group]
			sys.removeGroupFromMembershipsMap(group)
			sys.updateGroupMembershipsMap(group, &gi)
		case policyPrefix:
			policyName := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPoliciesPrefix))
			ies.loadPolicyDoc(ctx, policyName, sys.iamPolicyDocsMap)
		case policyDBUsersPrefix:
			policyMapFile := strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPolicyDBUsersPrefix)
			user := strings.TrimSuffix(policyMapFile, ".json")
			ies.loadMappedPolicy(ctx, user, regularUser, false, sys.iamUserPolicyMap)
		case policyDBSTSUsersPrefix:
			policyMapFile := strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPolicyDBSTSUsersPrefix)
			user := strings.TrimSuffix(policyMapFile, ".json")
			ies.loadMappedPolicy(ctx, user, stsUser, false, sys.iamUserPolicyMap)
		case policyDBGroupsPrefix:
			policyMapFile := strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPolicyDBGroupsPrefix)
			user := strings.TrimSuffix(policyMapFile, ".json")
			ies.loadMappedPolicy(ctx, user, regularUser, true, sys.iamGroupPolicyMap)
		}
	case eventDelete:
		switch {
		case usersPrefix:
			accessKey := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigUsersPrefix))
			delete(sys.iamUsersMap, accessKey)
		case stsPrefix:
			accessKey := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigSTSPrefix))
			delete(sys.iamUsersMap, accessKey)
		case groupsPrefix:
			group := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigGroupsPrefix))
			sys.removeGroupFromMembershipsMap(group)
			delete(sys.iamGroupsMap, group)
			delete(sys.iamGroupPolicyMap, group)
		case policyPrefix:
			policyName := path.Dir(strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPoliciesPrefix))
			delete(sys.iamPolicyDocsMap, policyName)
		case policyDBUsersPrefix:
			policyMapFile := strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPolicyDBUsersPrefix)
			user := strings.TrimSuffix(policyMapFile, ".json")
			delete(sys.iamUserPolicyMap, user)
		case policyDBSTSUsersPrefix:
			policyMapFile := strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPolicyDBSTSUsersPrefix)
			user := strings.TrimSuffix(policyMapFile, ".json")
			delete(sys.iamUserPolicyMap, user)
		case policyDBGroupsPrefix:
			policyMapFile := strings.TrimPrefix(string(event.Kv.Key),
				iamConfigPolicyDBGroupsPrefix)
			user := strings.TrimSuffix(policyMapFile, ".json")
			delete(sys.iamGroupPolicyMap, user)
		}
	}
}

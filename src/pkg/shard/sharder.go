package shard

import (
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/jsonpb"
	"github.com/pachyderm/pachyderm/src/pkg/discovery"
	"go.pedge.io/protolog"
)

const InvalidVersion int64 = -1

var (
	holdTTL      uint64 = 20
	marshaler           = &jsonpb.Marshaler{}
	ErrCancelled        = fmt.Errorf("cancelled by user")
	errComplete         = fmt.Errorf("COMPLETE")
)

type sharder struct {
	discoveryClient discovery.Client
	numShards       uint64
	numReplicas     uint64
	namespace       string
	addresses       map[int64]*Addresses
	addressesLock   sync.RWMutex
}

func newSharder(discoveryClient discovery.Client, numShards uint64, numReplicas uint64, namespace string) *sharder {
	return &sharder{discoveryClient, numShards, numReplicas, namespace, make(map[int64]*Addresses), sync.RWMutex{}}
}

func (a *sharder) GetMasterAddress(shard uint64, version int64) (result string, ok bool, retErr error) {
	defer func() {
		protolog.Debug(&GetMasterAddress{shard, version, result, ok, errorToString(retErr)})
	}()
	addresses, err := a.getAddresses(version)
	if err != nil {
		return "", false, err
	}
	shardAddresses, ok := addresses.Addresses[shard]
	if !ok {
		return "", false, nil
	}
	return shardAddresses.Master, true, nil
}

func (a *sharder) GetReplicaAddresses(shard uint64, version int64) (result map[string]bool, retErr error) {
	defer func() {
		protolog.Debug(&GetReplicaAddresses{shard, version, result, errorToString(retErr)})
	}()
	addresses, err := a.getAddresses(version)
	if err != nil {
		return nil, err
	}
	shardAddresses, ok := addresses.Addresses[shard]
	if !ok {
		return nil, fmt.Errorf("shard %d not found", shard)
	}
	return shardAddresses.Replicas, nil
}

func (a *sharder) GetShardToMasterAddress(version int64) (result map[uint64]string, retErr error) {
	defer func() {
		protolog.Debug(&GetShardToMasterAddress{version, result, errorToString(retErr)})
	}()
	addresses, err := a.getAddresses(version)
	if err != nil {
		return nil, err
	}
	_result := make(map[uint64]string)
	for shard, shardAddresses := range addresses.Addresses {
		_result[shard] = shardAddresses.Master
	}
	return _result, nil
}

func (a *sharder) GetShardToReplicaAddresses(version int64) (result map[uint64]map[string]bool, retErr error) {
	defer func() {
		// We need resultPrime is because proto3 can't do maps of maps.
		resultPrime := make(map[uint64]*ReplicaAddresses)
		for shard, addresses := range result {
			resultPrime[shard] = &ReplicaAddresses{addresses}
		}
		protolog.Debug(&GetShardToReplicaAddresses{version, resultPrime, errorToString(retErr)})
	}()
	addresses, err := a.getAddresses(version)
	if err != nil {
		return nil, err
	}
	_result := make(map[uint64]map[string]bool)
	for shard, shardAddresses := range addresses.Addresses {
		_result[shard] = shardAddresses.Replicas
	}
	return _result, nil
}

func (a *sharder) Register(cancel chan bool, address string, server Server) (retErr error) {
	protolog.Info(&StartRegister{address})
	defer func() {
		protolog.Info(&FinishRegister{address, errorToString(retErr)})
	}()
	var once sync.Once
	versionChan := make(chan int64)
	internalCancel := make(chan bool)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if err := a.announceServer(address, server, versionChan, internalCancel); err != nil {
			once.Do(func() {
				retErr = err
				close(internalCancel)
			})
		}
	}()
	go func() {
		defer wg.Done()
		if err := a.fillRoles(address, server, versionChan, internalCancel); err != nil {
			once.Do(func() {
				retErr = err
				close(internalCancel)
			})
		}
	}()
	go func() {
		defer wg.Done()
		select {
		case <-cancel:
			once.Do(func() {
				retErr = ErrCancelled
				close(internalCancel)
			})
		case <-internalCancel:
		}
	}()
	wg.Wait()
	return
}

func (a *sharder) RegisterFrontend(cancel chan bool, address string, frontend Frontend) (retErr error) {
	var once sync.Once
	versionChan := make(chan int64)
	internalCancel := make(chan bool)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if err := a.announceFrontend(address, frontend, versionChan, internalCancel); err != nil {
			once.Do(func() {
				retErr = err
				close(internalCancel)
			})
		}
	}()
	go func() {
		defer wg.Done()
		if err := a.runFrontend(address, frontend, versionChan, internalCancel); err != nil {
			once.Do(func() {
				retErr = err
				close(internalCancel)
			})
		}
	}()
	go func() {
		defer wg.Done()
		select {
		case <-cancel:
			once.Do(func() {
				retErr = ErrCancelled
				close(internalCancel)
			})
		case <-internalCancel:
		}
	}()
	wg.Wait()
	return
}

func (a *sharder) AssignRoles(cancel chan bool) (retErr error) {
	protolog.Info(&StartAssignRoles{})
	defer func() {
		protolog.Info(&FinishAssignRoles{errorToString(retErr)})
	}()
	var version int64
	oldServers := make(map[string]bool)
	oldRoles := make(map[string]*ServerRole)
	oldMasters := make(map[uint64]string)
	oldReplicas := make(map[uint64][]string)
	var oldMinVersion int64
	// Reconstruct state from a previous run
	serverRoles, err := a.discoveryClient.GetAll(a.serverRoleDir())
	if err != nil {
		return err
	}
	for _, encodedServerRole := range serverRoles {
		serverRole, err := decodeServerRole(encodedServerRole)
		if err != nil {
			return err
		}
		if oldServerRole, ok := oldRoles[serverRole.Address]; !ok || oldServerRole.Version < serverRole.Version {
			oldRoles[serverRole.Address] = serverRole
			oldServers[serverRole.Address] = true
		}
		if version < serverRole.Version+1 {
			version = serverRole.Version + 1
		}
	}
	for _, oldServerRole := range oldRoles {
		for shard := range oldServerRole.Masters {
			oldMasters[shard] = oldServerRole.Address
		}
		for shard := range oldServerRole.Replicas {
			oldReplicas[shard] = append(oldReplicas[shard], oldServerRole.Address)
		}
	}
	err = a.discoveryClient.WatchAll(a.serverStateDir(), cancel,
		func(encodedServerStates map[string]string) error {
			if len(encodedServerStates) == 0 {
				return nil
			}
			newServerStates := make(map[string]*ServerState)
			shardLocations := make(map[uint64][]string)
			newRoles := make(map[string]*ServerRole)
			newMasters := make(map[uint64]string)
			newReplicas := make(map[uint64][]string)
			masterRolesPerServer := a.numShards / uint64(len(encodedServerStates))
			masterRolesRemainder := a.numShards % uint64(len(encodedServerStates))
			replicaRolesPerServer := (a.numShards * a.numReplicas) / uint64(len(encodedServerStates))
			replicaRolesRemainder := (a.numShards * a.numReplicas) % uint64(len(encodedServerStates))
			for _, encodedServerState := range encodedServerStates {
				serverState, err := decodeServerState(encodedServerState)
				if err != nil {
					return err
				}
				newServerStates[serverState.Address] = serverState
				newRoles[serverState.Address] = &ServerRole{
					Address:  serverState.Address,
					Version:  version,
					Masters:  make(map[uint64]bool),
					Replicas: make(map[uint64]bool),
				}
				for shard := range serverState.Shards {
					shardLocations[shard] = append(shardLocations[shard], serverState.Address)
				}
			}
			// See if there's any roles we can delete
			minVersion := int64(math.MaxInt64)
			for _, serverState := range newServerStates {
				if serverState.Version < minVersion {
					minVersion = serverState.Version
				}
			}
			// Delete roles that no servers are using anymore
			if minVersion > oldMinVersion {
				oldMinVersion = minVersion
				if err := a.discoveryClient.WatchAll(
					a.frontendStateDir(),
					cancel,
					func(encodedFrontendStates map[string]string) error {
						for _, encodedFrontendState := range encodedFrontendStates {
							frontendState, err := decodeFrontendState(encodedFrontendState)
							if err != nil {
								return err
							}
							if frontendState.Version < minVersion {
								return nil
							}
						}
						return errComplete
					}); err != nil && err != errComplete {
					return err
				}
				serverRoles, err := a.discoveryClient.GetAll(a.serverRoleDir())
				if err != nil {
					return err
				}
				for key, encodedServerRole := range serverRoles {
					serverRole, err := decodeServerRole(encodedServerRole)
					if err != nil {
						return err
					}
					if serverRole.Version < minVersion {
						if err := a.discoveryClient.Delete(key); err != nil {
							return err
						}
						protolog.Info(&DeleteServerRole{serverRole})
					}
				}
			}
			// if the servers are identical to last time then we know we'll
			// assign shards the same way
			if sameServers(oldServers, newServerStates) {
				return nil
			}
		Master:
			for shard := uint64(0); shard < a.numShards; shard++ {
				if address, ok := oldMasters[shard]; ok {
					if assignMaster(newRoles, newMasters, address, shard, masterRolesPerServer, &masterRolesRemainder) {
						continue Master
					}
				}
				for _, address := range oldReplicas[shard] {
					if assignMaster(newRoles, newMasters, address, shard, masterRolesPerServer, &masterRolesRemainder) {
						continue Master
					}
				}
				for _, address := range shardLocations[shard] {
					if assignMaster(newRoles, newMasters, address, shard, masterRolesPerServer, &masterRolesRemainder) {
						continue Master
					}
				}
				for address := range newServerStates {
					if assignMaster(newRoles, newMasters, address, shard, masterRolesPerServer, &masterRolesRemainder) {
						continue Master
					}
				}
				protolog.Error(&FailedToAssignRoles{
					ServerStates: newServerStates,
					NumShards:    a.numShards,
					NumReplicas:  a.numReplicas,
				})
				return nil
			}
			for replica := uint64(0); replica < a.numReplicas; replica++ {
			Replica:
				for shard := uint64(0); shard < a.numShards; shard++ {
					if address, ok := oldMasters[shard]; ok {
						if assignReplica(newRoles, newMasters, newReplicas, address, shard, replicaRolesPerServer, &replicaRolesRemainder) {
							continue Replica
						}
					}
					for _, address := range oldReplicas[shard] {
						if assignReplica(newRoles, newMasters, newReplicas, address, shard, replicaRolesPerServer, &replicaRolesRemainder) {
							continue Replica
						}
					}
					for _, address := range shardLocations[shard] {
						if assignReplica(newRoles, newMasters, newReplicas, address, shard, replicaRolesPerServer, &replicaRolesRemainder) {
							continue Replica
						}
					}
					for address := range newServerStates {
						if assignReplica(newRoles, newMasters, newReplicas, address, shard, replicaRolesPerServer, &replicaRolesRemainder) {
							continue Replica
						}
					}
					for address := range newServerStates {
						if swapReplica(newRoles, newMasters, newReplicas, address, shard, replicaRolesPerServer) {
							continue Replica
						}
					}
					protolog.Error(&FailedToAssignRoles{
						ServerStates: newServerStates,
						NumShards:    a.numShards,
						NumReplicas:  a.numReplicas,
					})
					return nil
				}
			}
			addresses := Addresses{
				Version:   version,
				Addresses: make(map[uint64]*ShardAddresses),
			}
			for shard := uint64(0); shard < a.numShards; shard++ {
				addresses.Addresses[shard] = &ShardAddresses{Replicas: make(map[string]bool)}
			}
			for address, serverRole := range newRoles {
				encodedServerRole, err := marshaler.MarshalToString(serverRole)
				if err != nil {
					return err
				}
				if err := a.discoveryClient.Set(a.serverRoleKeyVersion(address, version), encodedServerRole, 0); err != nil {
					return err
				}
				protolog.Info(&SetServerRole{serverRole})
				address := newServerStates[address].Address
				for shard := range serverRole.Masters {
					shardAddresses := addresses.Addresses[shard]
					shardAddresses.Master = address
					addresses.Addresses[shard] = shardAddresses
				}
				for shard := range serverRole.Replicas {
					shardAddresses := addresses.Addresses[shard]
					shardAddresses.Replicas[address] = true
					addresses.Addresses[shard] = shardAddresses
				}
			}
			encodedAddresses, err := marshaler.MarshalToString(&addresses)
			if err != nil {
				return err
			}
			if err := a.discoveryClient.Set(a.addressesKey(version), encodedAddresses, 0); err != nil {
				return err
			}
			protolog.Info(&SetAddresses{&addresses})
			version++
			oldServers = make(map[string]bool)
			for address := range newServerStates {
				oldServers[address] = true
			}
			oldRoles = newRoles
			oldMasters = newMasters
			oldReplicas = newReplicas
			return nil
		})
	if err == discovery.ErrCancelled {
		return ErrCancelled
	}
	return err
}

func (a *sharder) WaitForAvailability(frontendAddresses []string, serverAddresses []string) error {
	version := InvalidVersion
	if err := a.discoveryClient.WatchAll(a.serverDir(), nil,
		func(encodedServerStatesAndRoles map[string]string) error {
			serverStates := make(map[string]*ServerState)
			serverRoles := make(map[string]map[int64]*ServerRole)
			for key, encodedServerStateOrRole := range encodedServerStatesAndRoles {
				if strings.HasPrefix(key, a.serverStateDir()) {
					serverState, err := decodeServerState(encodedServerStateOrRole)
					if err != nil {
						return err
					}
					serverStates[serverState.Address] = serverState
				}
				if strings.HasPrefix(key, a.serverRoleDir()) {
					serverRole, err := decodeServerRole(encodedServerStateOrRole)
					if err != nil {
						return err
					}
					if _, ok := serverRoles[serverRole.Address]; !ok {
						serverRoles[serverRole.Address] = make(map[int64]*ServerRole)
					}
					serverRoles[serverRole.Address][serverRole.Version] = serverRole
				}
			}
			if len(serverStates) != len(serverAddresses) {
				return nil
			}
			if len(serverRoles) != len(serverAddresses) {
				return nil
			}
			for _, address := range serverAddresses {
				if _, ok := serverStates[address]; !ok {
					return nil
				}
				if _, ok := serverRoles[address]; !ok {
					return nil
				}
			}
			versions := make(map[int64]bool)
			for _, serverState := range serverStates {
				if serverState.Version == InvalidVersion {
					return nil
				}
				versions[serverState.Version] = true
			}
			if len(versions) != 1 {
				return nil
			}
			for _, versionToServerRole := range serverRoles {
				if len(versionToServerRole) != 1 {
					return nil
				}
				for version := range versionToServerRole {
					if !versions[version] {
						return nil
					}
				}
			}
			// This loop actually does something, it sets the outside
			// version variable.
			for version = range versions {
			}
			return errComplete
		}); err != errComplete {
		return err
	}

	if err := a.discoveryClient.WatchAll(
		a.frontendStateDir(),
		nil,
		func(encodedFrontendStates map[string]string) error {
			frontendStates := make(map[string]*FrontendState)
			for _, encodedFrontendState := range encodedFrontendStates {
				frontendState, err := decodeFrontendState(encodedFrontendState)
				if err != nil {
					return err
				}

				if frontendState.Version != version {
					protolog.Printf("Wrong version: %d != %d", frontendState.Version, version)
					return nil
				}
				frontendStates[frontendState.Address] = frontendState
			}
			protolog.Printf("frontendStates: %+v", frontendStates)
			if len(frontendStates) != len(frontendAddresses) {
				return nil
			}
			for _, address := range frontendAddresses {
				if _, ok := frontendStates[address]; !ok {
					return nil
				}
			}
			return errComplete
		}); err != nil && err != errComplete {
		return err
	}
	return nil
}

func (a *sharder) routeDir() string {
	return fmt.Sprintf("%s/pfs/route", a.namespace)
}

func (a *sharder) serverDir() string {
	return path.Join(a.routeDir(), "server")
}

func (a *sharder) serverStateDir() string {
	return path.Join(a.serverDir(), "state")
}

func (a *sharder) serverStateKey(address string) string {
	return path.Join(a.serverStateDir(), address)
}

func (a *sharder) serverRoleDir() string {
	return path.Join(a.serverDir(), "role")
}

func (a *sharder) serverRoleKey(address string) string {
	return path.Join(a.serverRoleDir(), address)
}

func (a *sharder) serverRoleKeyVersion(address string, version int64) string {
	return path.Join(a.serverRoleKey(address), fmt.Sprint(version))
}

func (a *sharder) frontendDir() string {
	return path.Join(a.routeDir(), "frontend")
}

func (a *sharder) frontendStateDir() string {
	return path.Join(a.frontendDir(), "state")
}

func (a *sharder) frontendStateKey(address string) string {
	return path.Join(a.frontendStateDir(), address)
}

func (a *sharder) addressesDir() string {
	return path.Join(a.routeDir(), "addresses")
}

func (a *sharder) addressesKey(version int64) string {
	return path.Join(a.addressesDir(), fmt.Sprint(version))
}

func decodeServerState(encodedServerState string) (*ServerState, error) {
	var serverState ServerState
	if err := jsonpb.UnmarshalString(encodedServerState, &serverState); err != nil {
		return nil, err
	}
	return &serverState, nil
}

func decodeFrontendState(encodedFrontendState string) (*FrontendState, error) {
	var frontendState FrontendState
	if err := jsonpb.UnmarshalString(encodedFrontendState, &frontendState); err != nil {
		return nil, err
	}
	return &frontendState, nil
}

func (a *sharder) getServerStates() (map[string]*ServerState, error) {
	encodedServerStates, err := a.discoveryClient.GetAll(a.serverStateDir())
	if err != nil {
		return nil, err
	}
	result := make(map[string]*ServerState)
	for _, encodedServerState := range encodedServerStates {
		serverState, err := decodeServerState(encodedServerState)
		if err != nil {
			return nil, err
		}
		result[serverState.Address] = serverState
	}
	return result, nil
}

func (a *sharder) getServerState(address string) (*ServerState, error) {
	encodedServerState, err := a.discoveryClient.Get(a.serverStateKey(address))
	if err != nil {
		return nil, err
	}
	return decodeServerState(encodedServerState)
}

func decodeServerRole(encodedServerRole string) (*ServerRole, error) {
	var serverRole ServerRole
	if err := jsonpb.UnmarshalString(encodedServerRole, &serverRole); err != nil {
		return nil, err
	}
	return &serverRole, nil
}

func (a *sharder) getServerRoles() (map[string]map[int64]*ServerRole, error) {
	encodedServerRoles, err := a.discoveryClient.GetAll(a.serverRoleDir())
	if err != nil {
		return nil, err
	}
	result := make(map[string]map[int64]*ServerRole)
	for _, encodedServerRole := range encodedServerRoles {
		serverRole, err := decodeServerRole(encodedServerRole)
		if err != nil {
			return nil, err
		}
		if _, ok := result[serverRole.Address]; !ok {
			result[serverRole.Address] = make(map[int64]*ServerRole)
		}
		result[serverRole.Address][serverRole.Version] = serverRole
	}
	return result, nil
}

func (a *sharder) getServerRole(address string) (map[int64]*ServerRole, error) {
	encodedServerRoles, err := a.discoveryClient.GetAll(a.serverRoleKey(address))
	if err != nil {
		return nil, err
	}
	result := make(map[int64]*ServerRole)
	for _, encodedServerRole := range encodedServerRoles {
		serverRole, err := decodeServerRole(encodedServerRole)
		if err != nil {
			return nil, err
		}
		result[serverRole.Version] = serverRole
	}
	return result, nil
}

func (a *sharder) getAddresses(version int64) (*Addresses, error) {
	if version == InvalidVersion {
		return nil, fmt.Errorf("invalid version")
	}
	a.addressesLock.RLock()
	if addresses, ok := a.addresses[version]; ok {
		a.addressesLock.RUnlock()
		return addresses, nil
	}
	a.addressesLock.RUnlock()
	a.addressesLock.Lock()
	defer a.addressesLock.Unlock()
	encodedAddresses, err := a.discoveryClient.Get(a.addressesKey(version))
	if err != nil {
		return nil, err
	}
	var addresses Addresses
	if err := jsonpb.UnmarshalString(encodedAddresses, &addresses); err != nil {
		return nil, err
	}
	a.addresses[version] = &addresses
	return &addresses, nil
}

func hasShard(serverRole *ServerRole, shard uint64) bool {
	return serverRole.Masters[shard] || serverRole.Replicas[shard]
}

func removeReplica(replicas map[uint64][]string, shard uint64, address string) {
	var addresses []string
	for _, replicaAddress := range replicas[shard] {
		if address != replicaAddress {
			addresses = append(addresses, replicaAddress)
		}
	}
	replicas[shard] = addresses
}

func assignMaster(
	serverRoles map[string]*ServerRole,
	masters map[uint64]string,
	address string,
	shard uint64,
	masterRolesPerServer uint64,
	masterRolesRemainder *uint64,
) bool {
	serverRole, ok := serverRoles[address]
	if !ok {
		return false
	}
	if uint64(len(serverRole.Masters)) > masterRolesPerServer {
		return false
	}
	if uint64(len(serverRole.Masters)) == masterRolesPerServer && *masterRolesRemainder == 0 {
		return false
	}
	if hasShard(serverRole, shard) {
		return false
	}
	if uint64(len(serverRole.Masters)) == masterRolesPerServer && *masterRolesRemainder > 0 {
		*masterRolesRemainder--
	}
	serverRole.Masters[shard] = true
	serverRoles[address] = serverRole
	masters[shard] = address
	return true
}

func assignReplica(
	serverRoles map[string]*ServerRole,
	masters map[uint64]string,
	replicas map[uint64][]string,
	address string,
	shard uint64,
	replicaRolesPerServer uint64,
	replicaRolesRemainder *uint64,
) bool {
	serverRole, ok := serverRoles[address]
	if !ok {
		return false
	}
	if uint64(len(serverRole.Replicas)) > replicaRolesPerServer {
		return false
	}
	if uint64(len(serverRole.Replicas)) == replicaRolesPerServer && *replicaRolesRemainder == 0 {
		return false
	}
	if hasShard(serverRole, shard) {
		return false
	}
	if uint64(len(serverRole.Replicas)) == replicaRolesPerServer && *replicaRolesRemainder > 0 {
		*replicaRolesRemainder--
	}
	serverRole.Replicas[shard] = true
	serverRoles[address] = serverRole
	replicas[shard] = append(replicas[shard], address)
	return true
}

func swapReplica(
	serverRoles map[string]*ServerRole,
	masters map[uint64]string,
	replicas map[uint64][]string,
	address string,
	shard uint64,
	replicaRolesPerServer uint64,
) bool {
	serverRole, ok := serverRoles[address]
	if !ok {
		return false
	}
	if uint64(len(serverRole.Replicas)) >= replicaRolesPerServer {
		return false
	}
	for swapID, swapServerRole := range serverRoles {
		if swapID == address {
			continue
		}
		for swapShard := range swapServerRole.Replicas {
			if hasShard(serverRole, swapShard) {
				continue
			}
			if hasShard(swapServerRole, shard) {
				continue
			}
			delete(swapServerRole.Replicas, swapShard)
			serverRoles[swapID] = swapServerRole
			removeReplica(replicas, swapShard, swapID)
			// We do some weird things with the limits here, both servers
			// receive a 0 replicaRolesRemainder, swapID doesn't need a
			// remainder because we're replacing a shard we stole so it also
			// has MaxInt64 for replicaRolesPerServer. We already know address
			// doesn't need the remainder since we check that it has fewer than
			// replicaRolesPerServer replicas.
			var noReplicaRemainder uint64
			assignReplica(serverRoles, masters, replicas, swapID, shard, math.MaxUint64, &noReplicaRemainder)
			assignReplica(serverRoles, masters, replicas, address, swapShard, replicaRolesPerServer, &noReplicaRemainder)
			return true
		}
	}
	return false
}

func (a *sharder) announceServer(
	address string,
	server Server,
	versionChan chan int64,
	cancel chan bool,
) error {
	serverState := &ServerState{
		Address: address,
		Version: InvalidVersion,
	}
	for {
		shards, err := server.LocalShards()
		if err != nil {
			return err
		}
		serverState.Shards = shards
		encodedServerState, err := marshaler.MarshalToString(serverState)
		if err != nil {
			return err
		}
		if err := a.discoveryClient.Set(a.serverStateKey(address), encodedServerState, holdTTL); err != nil {
			protolog.Printf("Error setting server state: %s", err.Error())
		}
		protolog.Debug(&SetServerState{serverState})
		select {
		case <-cancel:
			return nil
		case version := <-versionChan:
			serverState.Version = version
		case <-time.After(time.Second * time.Duration(holdTTL/2)):
		}
	}
}

func (a *sharder) announceFrontend(
	address string,
	frontend Frontend,
	versionChan chan int64,
	cancel chan bool,
) error {
	frontendState := &FrontendState{
		Address: address,
		Version: InvalidVersion,
	}
	for {
		encodedFrontendState, err := marshaler.MarshalToString(frontendState)
		if err != nil {
			return err
		}
		if err := a.discoveryClient.Set(a.frontendStateKey(address), encodedFrontendState, holdTTL); err != nil {
			protolog.Printf("Error setting server state: %s", err.Error())
		}
		protolog.Debug(&SetFrontendState{frontendState})
		select {
		case <-cancel:
			return nil
		case version := <-versionChan:
			frontendState.Version = version
		case <-time.After(time.Second * time.Duration(holdTTL/2)):
		}
	}
}

type int64Slice []int64

func (s int64Slice) Len() int           { return len(s) }
func (s int64Slice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s int64Slice) Less(i, j int) bool { return s[i] < s[j] }

func (a *sharder) fillRoles(
	address string,
	server Server,
	versionChan chan int64,
	cancel chan bool,
) error {
	oldRoles := make(map[int64]ServerRole)
	return a.discoveryClient.WatchAll(
		a.serverRoleKey(address),
		cancel,
		func(encodedServerRoles map[string]string) error {
			roles := make(map[int64]ServerRole)
			var versions int64Slice
			// Decode the roles
			for _, encodedServerRole := range encodedServerRoles {
				var serverRole ServerRole
				if err := jsonpb.UnmarshalString(encodedServerRole, &serverRole); err != nil {
					return err
				}
				roles[serverRole.Version] = serverRole
				versions = append(versions, serverRole.Version)
			}
			sort.Sort(versions)
			if len(versions) > 2 {
				versions = versions[0:2]
			}
			// For each new version bring the server up to date
			for _, version := range versions {
				if _, ok := oldRoles[version]; ok {
					// we've already seen these roles, so nothing to do here
					continue
				}
				serverRole := roles[version]
				var wg sync.WaitGroup
				var addShardErr error
				for _, shard := range shards(serverRole) {
					if !containsShard(oldRoles, shard) {
						wg.Add(1)
						shard := shard
						go func() {
							defer wg.Done()
							if err := server.AddShard(shard, version-1); err != nil && addShardErr == nil {
								addShardErr = err
							}
						}()
					}
				}
				wg.Wait()
				if addShardErr != nil {
					protolog.Info(&AddServerRole{&serverRole, addShardErr.Error()})
					return addShardErr
				}
				protolog.Info(&AddServerRole{&serverRole, ""})
				oldRoles[version] = serverRole
				versionChan <- version
			}
			// See if there are any old roles that aren't needed
			for version, serverRole := range oldRoles {
				var wg sync.WaitGroup
				var removeShardErr error
				if _, ok := roles[version]; ok {
					// these roles haven't expired yet, so nothing to do
					continue
				}
				for _, shard := range shards(serverRole) {
					if !containsShard(roles, shard) {
						wg.Add(1)
						shard := shard
						go func(shard uint64) {
							defer wg.Done()
							if err := server.RemoveShard(shard, version-1); err != nil && removeShardErr == nil {
								removeShardErr = err
							}
						}(shard)
					}
				}
				wg.Wait()
				if removeShardErr != nil {
					protolog.Info(&RemoveServerRole{&serverRole, removeShardErr.Error()})
					return removeShardErr
				}
				protolog.Info(&RemoveServerRole{&serverRole, ""})
			}
			oldRoles = make(map[int64]ServerRole)
			for _, version := range versions {
				oldRoles[version] = roles[version]
			}
			return nil
		},
	)
}

func (a *sharder) runFrontend(
	address string,
	frontend Frontend,
	versionChan chan int64,
	cancel chan bool,
) error {
	version := InvalidVersion
	return a.discoveryClient.WatchAll(
		a.serverStateDir(),
		cancel,
		func(encodedServerStates map[string]string) error {
			if len(encodedServerStates) == 0 {
				return nil
			}
			minVersion := int64(math.MaxInt64)
			for _, encodedServerState := range encodedServerStates {
				serverState, err := decodeServerState(encodedServerState)
				if err != nil {
					return err
				}
				if serverState.Version < minVersion {
					minVersion = serverState.Version
				}
			}
			if minVersion > version {
				if err := frontend.Version(minVersion); err != nil {
					return err
				}
				version = minVersion
				versionChan <- version
			}
			return nil
		})
}

func shards(serverRole ServerRole) []uint64 {
	var result []uint64
	for shard := range serverRole.Masters {
		result = append(result, shard)
	}
	for shard := range serverRole.Replicas {
		result = append(result, shard)
	}
	return result
}

func containsShard(roles map[int64]ServerRole, shard uint64) bool {
	for _, serverRole := range roles {
		if serverRole.Masters[shard] || serverRole.Replicas[shard] {
			return true
		}
	}
	return false
}

func sameServers(oldServers map[string]bool, newServerStates map[string]*ServerState) bool {
	if len(oldServers) != len(newServerStates) {
		return false
	}
	for address := range oldServers {
		if _, ok := newServerStates[address]; !ok {
			return false
		}
	}
	return true
}

// TODO this code is duplicate elsewhere, we should put it somehwere.
func errorToString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

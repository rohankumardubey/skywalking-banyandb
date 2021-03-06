// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package schema

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
	"google.golang.org/protobuf/proto"

	commonv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/common/v1"
	databasev1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/database/v1"
)

var (
	_ Stream           = (*etcdSchemaRegistry)(nil)
	_ IndexRuleBinding = (*etcdSchemaRegistry)(nil)
	_ IndexRule        = (*etcdSchemaRegistry)(nil)
	_ Measure          = (*etcdSchemaRegistry)(nil)
	_ Group            = (*etcdSchemaRegistry)(nil)

	ErrGroupAbsent                = errors.New("group is absent")
	ErrEntityNotFound             = errors.New("entity is not found")
	ErrUnexpectedNumberOfEntities = errors.New("unexpected number of entities")
	ErrConcurrentModification     = errors.New("concurrent modification of entities")

	unixDomainSockScheme = "unix"

	GroupsKeyPrefix           = "/groups/"
	GroupMetadataKey          = "/__meta_group__"
	StreamKeyPrefix           = "/streams/"
	IndexRuleBindingKeyPrefix = "/index-rule-bindings/"
	IndexRuleKeyPrefix        = "/index-rules/"
	MeasureKeyPrefix          = "/measures/"
)

type HasMetadata interface {
	GetMetadata() *commonv1.Metadata
	proto.Message
}

type RegistryOption func(*etcdSchemaRegistryConfig)

func RootDir(rootDir string) RegistryOption {
	return func(config *etcdSchemaRegistryConfig) {
		config.rootDir = rootDir
	}
}

func randomUnixDomainListener() (string, string) {
	i := rand.Uint64()
	return fmt.Sprintf("%s://localhost:%d%06d", unixDomainSockScheme, os.Getpid(), i),
		fmt.Sprintf("%s://localhost:%d%06d", unixDomainSockScheme, os.Getpid(), i+1)
}

func UseRandomListener() RegistryOption {
	return func(config *etcdSchemaRegistryConfig) {
		lc, lp := randomUnixDomainListener()
		config.listenerClientURL = lc
		config.listenerPeerURL = lp
	}
}

type eventHandler struct {
	interestKeys Kind
	handler      EventHandler
}

func (eh *eventHandler) InterestOf(kind Kind) bool {
	return KindMask&kind&eh.interestKeys != 0
}

type etcdSchemaRegistry struct {
	server   *embed.Etcd
	kv       clientv3.KV
	handlers []*eventHandler
}

type etcdSchemaRegistryConfig struct {
	// rootDir is the root directory for etcd storage
	rootDir string
	// listenerClientURL is the listener for client
	listenerClientURL string
	// listenerPeerURL is the listener for peer
	listenerPeerURL string
}

func (e *etcdSchemaRegistry) RegisterHandler(kind Kind, handler EventHandler) {
	e.handlers = append(e.handlers, &eventHandler{
		interestKeys: kind,
		handler:      handler,
	})
}

func (e *etcdSchemaRegistry) notifyUpdate(metadata Metadata) {
	for _, h := range e.handlers {
		if h.InterestOf(metadata.Kind) {
			h.handler.OnAddOrUpdate(metadata)
		}
	}
}

func (e *etcdSchemaRegistry) notifyDelete(metadata Metadata) {
	for _, h := range e.handlers {
		if h.InterestOf(metadata.Kind) {
			h.handler.OnDelete(metadata)
		}
	}
}

func (e *etcdSchemaRegistry) GetGroup(ctx context.Context, group string) (*commonv1.Group, error) {
	var entity commonv1.Group
	err := e.get(ctx, formatGroupKey(group), &entity)
	if err != nil {
		return nil, err
	}
	return &entity, nil
}

func (e *etcdSchemaRegistry) ListGroup(ctx context.Context) ([]*commonv1.Group, error) {
	messages, err := e.kv.Get(ctx, GroupsKeyPrefix, clientv3.WithFromKey(), clientv3.WithRange(incrementLastByte(GroupsKeyPrefix)))
	if err != nil {
		return nil, err
	}

	var groups []*commonv1.Group
	for _, kv := range messages.Kvs {
		// kv.Key = "/groups/" + {group} + "/__meta_info__"
		if strings.HasSuffix(string(kv.Key), GroupMetadataKey) {
			message := &commonv1.Group{}
			if innerErr := proto.Unmarshal(kv.Value, message); innerErr != nil {
				return nil, innerErr
			}
			groups = append(groups, message)
		}
	}

	return groups, nil
}

func (e *etcdSchemaRegistry) DeleteGroup(ctx context.Context, group string) (bool, error) {
	g, err := e.GetGroup(ctx, group)
	if err != nil {
		return false, errors.Wrap(err, group)
	}
	keyPrefix := GroupsKeyPrefix + g.GetMetadata().GetName() + "/"
	resp, err := e.kv.Delete(ctx, keyPrefix, clientv3.WithRange(incrementLastByte(keyPrefix)))
	if err != nil {
		return false, err
	}
	if resp.Deleted > 0 {
		e.notifyDelete(Metadata{
			TypeMeta: TypeMeta{
				Kind: KindGroup,
				Name: group,
			},
			Spec: g,
		})
	}

	return true, nil
}

func (e *etcdSchemaRegistry) UpdateGroup(ctx context.Context, group *commonv1.Group) error {
	return e.update(ctx, Metadata{
		TypeMeta: TypeMeta{
			Kind: KindGroup,
			Name: group.GetMetadata().GetName(),
		},
		Spec: group,
	})
}

func (e *etcdSchemaRegistry) GetMeasure(ctx context.Context, metadata *commonv1.Metadata) (*databasev1.Measure, error) {
	var entity databasev1.Measure
	if err := e.get(ctx, formatMeasureKey(metadata), &entity); err != nil {
		return nil, err
	}
	return &entity, nil
}

func (e *etcdSchemaRegistry) ListMeasure(ctx context.Context, opt ListOpt) ([]*databasev1.Measure, error) {
	if opt.Group == "" {
		return nil, errors.Wrap(ErrGroupAbsent, "list measure")
	}
	messages, err := e.listWithPrefix(ctx, listPrefixesForEntity(opt.Group, MeasureKeyPrefix), func() proto.Message {
		return &databasev1.Measure{}
	})
	if err != nil {
		return nil, err
	}
	entities := make([]*databasev1.Measure, 0, len(messages))
	for _, message := range messages {
		entities = append(entities, message.(*databasev1.Measure))
	}
	return entities, nil
}

func (e *etcdSchemaRegistry) UpdateMeasure(ctx context.Context, measure *databasev1.Measure) error {
	return e.update(ctx, Metadata{
		TypeMeta: TypeMeta{
			Kind:  KindMeasure,
			Group: measure.GetMetadata().GetGroup(),
			Name:  measure.GetMetadata().GetName(),
		},
		Spec: measure,
	})
}

func (e *etcdSchemaRegistry) DeleteMeasure(ctx context.Context, metadata *commonv1.Metadata) (bool, error) {
	return e.delete(ctx, Metadata{
		TypeMeta: TypeMeta{
			Kind:  KindMeasure,
			Group: metadata.GetGroup(),
			Name:  metadata.GetName(),
		},
	})
}

func (e *etcdSchemaRegistry) GetStream(ctx context.Context, metadata *commonv1.Metadata) (*databasev1.Stream, error) {
	var entity databasev1.Stream
	if err := e.get(ctx, formatStreamKey(metadata), &entity); err != nil {
		return nil, err
	}
	return &entity, nil
}

func (e *etcdSchemaRegistry) ListStream(ctx context.Context, opt ListOpt) ([]*databasev1.Stream, error) {
	if opt.Group == "" {
		return nil, errors.Wrap(ErrGroupAbsent, "list stream")
	}
	messages, err := e.listWithPrefix(ctx, listPrefixesForEntity(opt.Group, StreamKeyPrefix), func() proto.Message {
		return &databasev1.Stream{}
	})
	if err != nil {
		return nil, err
	}
	entities := make([]*databasev1.Stream, 0, len(messages))
	for _, message := range messages {
		entities = append(entities, message.(*databasev1.Stream))
	}
	return entities, nil
}

func (e *etcdSchemaRegistry) UpdateStream(ctx context.Context, stream *databasev1.Stream) error {
	return e.update(ctx, Metadata{
		TypeMeta: TypeMeta{
			Kind:  KindStream,
			Group: stream.GetMetadata().GetGroup(),
			Name:  stream.GetMetadata().GetName(),
		},
		Spec: stream,
	})
}

func (e *etcdSchemaRegistry) DeleteStream(ctx context.Context, metadata *commonv1.Metadata) (bool, error) {
	return e.delete(ctx, Metadata{
		TypeMeta: TypeMeta{
			Kind:  KindStream,
			Group: metadata.GetGroup(),
			Name:  metadata.GetName(),
		},
	})
}

func (e *etcdSchemaRegistry) GetIndexRuleBinding(ctx context.Context, metadata *commonv1.Metadata) (*databasev1.IndexRuleBinding, error) {
	var indexRuleBinding databasev1.IndexRuleBinding
	if err := e.get(ctx, formatIndexRuleBindingKey(metadata), &indexRuleBinding); err != nil {
		return nil, err
	}
	return &indexRuleBinding, nil
}

func (e *etcdSchemaRegistry) ListIndexRuleBinding(ctx context.Context, opt ListOpt) ([]*databasev1.IndexRuleBinding, error) {
	if opt.Group == "" {
		return nil, errors.Wrap(ErrGroupAbsent, "list index rule binding")
	}
	messages, err := e.listWithPrefix(ctx, listPrefixesForEntity(opt.Group, IndexRuleBindingKeyPrefix), func() proto.Message {
		return &databasev1.IndexRuleBinding{}
	})
	if err != nil {
		return nil, err
	}
	entities := make([]*databasev1.IndexRuleBinding, 0, len(messages))
	for _, message := range messages {
		entities = append(entities, message.(*databasev1.IndexRuleBinding))
	}
	return entities, nil
}

func (e *etcdSchemaRegistry) UpdateIndexRuleBinding(ctx context.Context, indexRuleBinding *databasev1.IndexRuleBinding) error {
	return e.update(ctx, Metadata{
		TypeMeta: TypeMeta{
			Kind:  KindIndexRuleBinding,
			Name:  indexRuleBinding.GetMetadata().GetName(),
			Group: indexRuleBinding.GetMetadata().GetGroup(),
		},
		Spec: indexRuleBinding,
	})
}

func (e *etcdSchemaRegistry) DeleteIndexRuleBinding(ctx context.Context, metadata *commonv1.Metadata) (bool, error) {
	return e.delete(ctx, Metadata{
		TypeMeta: TypeMeta{
			Kind:  KindIndexRuleBinding,
			Name:  metadata.GetName(),
			Group: metadata.GetGroup(),
		},
	})
}

func (e *etcdSchemaRegistry) GetIndexRule(ctx context.Context, metadata *commonv1.Metadata) (*databasev1.IndexRule, error) {
	var entity databasev1.IndexRule
	if err := e.get(ctx, formatIndexRuleKey(metadata), &entity); err != nil {
		return nil, err
	}
	return &entity, nil
}

func (e *etcdSchemaRegistry) ListIndexRule(ctx context.Context, opt ListOpt) ([]*databasev1.IndexRule, error) {
	if opt.Group == "" {
		return nil, errors.Wrap(ErrGroupAbsent, "list index rule")
	}
	messages, err := e.listWithPrefix(ctx, listPrefixesForEntity(opt.Group, IndexRuleKeyPrefix), func() proto.Message {
		return &databasev1.IndexRule{}
	})
	if err != nil {
		return nil, err
	}
	entities := make([]*databasev1.IndexRule, 0, len(messages))
	for _, message := range messages {
		entities = append(entities, message.(*databasev1.IndexRule))
	}
	return entities, nil
}

func (e *etcdSchemaRegistry) UpdateIndexRule(ctx context.Context, indexRule *databasev1.IndexRule) error {
	return e.update(ctx, Metadata{
		TypeMeta: TypeMeta{
			Kind:  KindIndexRule,
			Name:  indexRule.GetMetadata().GetName(),
			Group: indexRule.GetMetadata().GetGroup(),
		},
		Spec: indexRule,
	})
}

func (e *etcdSchemaRegistry) DeleteIndexRule(ctx context.Context, metadata *commonv1.Metadata) (bool, error) {
	return e.delete(ctx, Metadata{
		TypeMeta: TypeMeta{
			Kind:  KindIndexRule,
			Name:  metadata.GetName(),
			Group: metadata.GetGroup(),
		},
	})
}

func (e *etcdSchemaRegistry) ReadyNotify() <-chan struct{} {
	return e.server.Server.ReadyNotify()
}

func (e *etcdSchemaRegistry) StopNotify() <-chan struct{} {
	return e.server.Server.StopNotify()
}

func (e *etcdSchemaRegistry) StoppingNotify() <-chan struct{} {
	return e.server.Server.StoppingNotify()
}

func (e *etcdSchemaRegistry) Close() error {
	e.server.Close()
	return nil
}

func NewEtcdSchemaRegistry(options ...RegistryOption) (Registry, error) {
	registryConfig := &etcdSchemaRegistryConfig{
		rootDir:           os.TempDir(),
		listenerClientURL: embed.DefaultListenClientURLs,
		listenerPeerURL:   embed.DefaultListenPeerURLs,
	}
	for _, opt := range options {
		opt(registryConfig)
	}
	// TODO: allow use cluster setting
	embedConfig := newStandaloneEtcdConfig(registryConfig)
	e, err := embed.StartEtcd(embedConfig)
	if err != nil {
		return nil, err
	}
	if e != nil {
		<-e.Server.ReadyNotify() // wait for e.Server to join the cluster
	}
	client, err := clientv3.NewFromURL(e.Config().ACUrls[0].String())
	if err != nil {
		return nil, err
	}
	kvClient := clientv3.NewKV(client)
	reg := &etcdSchemaRegistry{
		server: e,
		kv:     kvClient,
	}
	return reg, nil
}

func (e *etcdSchemaRegistry) get(ctx context.Context, key string, message proto.Message) error {
	resp, err := e.kv.Get(ctx, key)
	if err != nil {
		return err
	}
	if resp.Count == 0 {
		return ErrEntityNotFound
	}
	if resp.Count > 1 {
		return ErrUnexpectedNumberOfEntities
	}
	if err = proto.Unmarshal(resp.Kvs[0].Value, message); err != nil {
		return err
	}
	if messageWithMetadata, ok := message.(HasMetadata); ok {
		// Assign readonly fields
		messageWithMetadata.GetMetadata().CreateRevision = resp.Kvs[0].CreateRevision
		messageWithMetadata.GetMetadata().ModRevision = resp.Kvs[0].ModRevision
	}
	return nil
}

func (e *etcdSchemaRegistry) update(ctx context.Context, metadata Metadata) error {
	key, err := metadata.Key()
	if err != nil {
		return err
	}
	getResp, err := e.kv.Get(ctx, key)
	if err != nil {
		return err
	}
	if getResp.Count > 1 {
		return ErrUnexpectedNumberOfEntities
	}
	val, err := proto.Marshal(metadata.Spec.(proto.Message))
	if err != nil {
		return err
	}
	replace := getResp.Count > 0
	if replace {
		existingVal, innerErr := metadata.Unmarshal(getResp.Kvs[0].Value)
		if innerErr != nil {
			return innerErr
		}
		// directly return if we have the same entity
		if metadata.Equal(existingVal) {
			return nil
		}

		modRevision := getResp.Kvs[0].ModRevision
		txnResp, txnErr := e.kv.Txn(context.Background()).
			If(clientv3.Compare(clientv3.ModRevision(key), "=", modRevision)).
			Then(clientv3.OpPut(key, string(val))).
			Commit()
		if txnErr != nil {
			return txnErr
		}
		if !txnResp.Succeeded {
			return ErrConcurrentModification
		}
	} else {
		_, err = e.kv.Put(ctx, key, string(val))
		if err != nil {
			return err
		}
	}
	e.notifyUpdate(metadata)
	return nil
}

func (e *etcdSchemaRegistry) listWithPrefix(ctx context.Context, prefix string, factory func() proto.Message) ([]proto.Message, error) {
	resp, err := e.kv.Get(ctx, prefix, clientv3.WithFromKey(), clientv3.WithRange(incrementLastByte(prefix)))
	if err != nil {
		return nil, err
	}
	entities := make([]proto.Message, resp.Count)
	for i := int64(0); i < resp.Count; i++ {
		message := factory()
		if innerErr := proto.Unmarshal(resp.Kvs[i].Value, message); innerErr != nil {
			return nil, innerErr
		}
		entities[i] = message
		if messageWithMetadata, ok := message.(HasMetadata); ok {
			// Assign readonly fields
			messageWithMetadata.GetMetadata().CreateRevision = resp.Kvs[i].CreateRevision
			messageWithMetadata.GetMetadata().ModRevision = resp.Kvs[i].ModRevision
		}
	}
	return entities, nil
}

func listPrefixesForEntity(group, entityPrefix string) string {
	return GroupsKeyPrefix + group + entityPrefix
}

func (e *etcdSchemaRegistry) delete(ctx context.Context, metadata Metadata) (bool, error) {
	key, err := metadata.Key()
	if err != nil {
		return false, err
	}
	resp, err := e.kv.Delete(ctx, key, clientv3.WithPrevKV())
	if err != nil {
		return false, err
	}
	if resp.Deleted == 1 {
		var message proto.Message
		switch metadata.Kind {
		case KindMeasure:
			message = &databasev1.Measure{}
		case KindStream:
			message = &databasev1.Stream{}
		case KindIndexRuleBinding:
			message = &databasev1.IndexRuleBinding{}
		case KindIndexRule:
			message = &databasev1.IndexRule{}
		}
		if unmarshalErr := proto.Unmarshal(resp.PrevKvs[0].Value, message); unmarshalErr == nil {
			e.notifyDelete(Metadata{
				TypeMeta: TypeMeta{
					Kind:  metadata.Kind,
					Name:  metadata.Name,
					Group: metadata.Group,
				},
				Spec: message,
			})
		}
		return true, nil
	}
	return false, nil
}

func formatIndexRuleKey(metadata *commonv1.Metadata) string {
	return formatKey(IndexRuleKeyPrefix, metadata)
}

func formatIndexRuleBindingKey(metadata *commonv1.Metadata) string {
	return formatKey(IndexRuleBindingKeyPrefix, metadata)
}

func formatStreamKey(metadata *commonv1.Metadata) string {
	return formatKey(StreamKeyPrefix, metadata)
}

func formatMeasureKey(metadata *commonv1.Metadata) string {
	return formatKey(MeasureKeyPrefix, metadata)
}

func formatKey(entityPrefix string, metadata *commonv1.Metadata) string {
	return GroupsKeyPrefix + metadata.GetGroup() + entityPrefix + metadata.GetName()
}

func formatGroupKey(group string) string {
	return GroupsKeyPrefix + group + GroupMetadataKey
}

func incrementLastByte(key string) string {
	bb := []byte(key)
	bb[len(bb)-1]++
	return string(bb)
}

func newStandaloneEtcdConfig(config *etcdSchemaRegistryConfig) *embed.Config {
	cfg := embed.NewConfig()
	// TODO: allow user to set path
	cfg.Dir = filepath.Join(config.rootDir, "metadata")
	cURL, _ := url.Parse(config.listenerClientURL)
	pURL, _ := url.Parse(config.listenerPeerURL)

	cfg.ClusterState = "new"
	cfg.LCUrls, cfg.ACUrls = []url.URL{*cURL}, []url.URL{*cURL}
	cfg.LPUrls, cfg.APUrls = []url.URL{*pURL}, []url.URL{*pURL}
	cfg.InitialCluster = ",default=" + pURL.String()
	return cfg
}

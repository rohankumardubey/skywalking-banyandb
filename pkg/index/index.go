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

package index

import (
	"bytes"

	modelv2 "github.com/apache/skywalking-banyandb/api/proto/banyandb/model/v2"
	"github.com/apache/skywalking-banyandb/pkg/index/posting"
)

type Field struct {
	Key  []byte
	Term []byte
}

func (f Field) Marshal() []byte {
	return bytes.Join([][]byte{f.Key, f.Term}, nil)
}

type RangeOpts struct {
	Upper         []byte
	Lower         []byte
	IncludesUpper bool
	IncludesLower bool
}

type FieldIterator interface {
	Next() bool
	Val() *PostingValue
	Close() error
}

type PostingValue struct {
	Key   []byte
	Value posting.List
}

type Searcher interface {
	MatchField(fieldName []byte) (list posting.List)
	MatchTerms(field Field) (list posting.List)
	Range(fieldName []byte, opts RangeOpts) (list posting.List)
	FieldIterator(fieldName []byte, order modelv2.QueryOrder_Sort) FieldIterator
}

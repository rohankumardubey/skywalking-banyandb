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

package stream

import (
	"encoding/base64"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/common/v1"
	modelv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/model/v1"
	streamv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/stream/v1"
)

var _ = Describe("Write", func() {
	var (
		s       *stream
		deferFn func()
	)

	BeforeEach(func() {
		var svcs *services
		svcs, deferFn = setUp()
		var ok bool
		s, ok = svcs.stream.schemaRepo.loadStream(&commonv1.Metadata{
			Name:  "sw",
			Group: "default",
		})
		Expect(ok).To(BeTrue())
	})

	AfterEach(func() {
		deferFn()
	})

	type args struct {
		ele *streamv1.ElementValue
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "golden path",
			args: args{
				ele: getEle(
					"trace_id-xxfff.111323",
					0,
					"webapp_id",
					"10.0.0.1_id",
					"/home_id",
					300,
					1622933202000000000,
				),
			},
		},
		{
			name: "minimal",
			args: args{
				ele: getEle(
					nil,
					1,
					"webapp_id",
					"10.0.0.1_id",
				),
			},
		},
		{
			name: "http",
			args: args{
				ele: getEle(
					"trace_id-xxfff.111323",
					0,
					"webapp_id",
					"10.0.0.1_id",
					"/home_id",
					300,
					1622933202000000000,
					"GET",
					"200",
				),
			},
		},
		{
			name: "database",
			args: args{
				ele: getEle(
					"trace_id-xxfff.111323",
					0,
					"webapp_id",
					"10.0.0.1_id",
					"/home_id",
					300,
					1622933202000000000,
					nil,
					nil,
					"MySQL",
					"10.1.1.2",
				),
			},
		},
		{
			name: "mq",
			args: args{
				ele: getEle(
					"trace_id-xxfff.111323",
					1,
					"webapp_id",
					"10.0.0.1_id",
					"/home_id",
					300,
					1622933202000000000,
					nil,
					nil,
					nil,
					nil,
					"test_topic",
					"10.0.0.1",
					"broker",
				),
			},
		},
		{
			name: "invalid trace id",
			args: args{
				ele: getEle(
					1212323,
					1,
					"webapp_id",
					"10.0.0.1_id",
				),
			},
			wantErr: true,
		},
		{
			name:    "empty input",
			args:    args{},
			wantErr: true,
		},
		{
			name: "unknown tags",
			args: args{
				ele: getEle(
					"trace_id-xxfff.111323",
					1,
					"webapp_id",
					"10.0.0.1_id",
					"/home_id",
					300,
					1622933202000000000,
					nil,
					nil,
					nil,
					nil,
					"test_topic",
					"10.0.0.1",
					"broker",
					"unknown",
				),
			},
			wantErr: true,
		},
	}
	Context("Writing stream", func() {
		for _, tt := range tests {
			It(tt.name, func() {
				err := s.Write(tt.args.ele)
				if tt.wantErr {
					Expect(err).Should(HaveOccurred())
					return
				}
				Expect(err).ShouldNot(HaveOccurred())
			})
		}
	})
})

func getEle(tags ...interface{}) *streamv1.ElementValue {
	searchableTags := make([]*modelv1.TagValue, 0)
	for _, tag := range tags {
		searchableTags = append(searchableTags, getTag(tag))
	}
	bb, _ := base64.StdEncoding.DecodeString("YWJjMTIzIT8kKiYoKSctPUB+")
	e := &streamv1.ElementValue{
		ElementId: "1231.dfd.123123ssf",
		Timestamp: timestamppb.Now(),
		TagFamilies: []*modelv1.TagFamilyForWrite{
			{
				Tags: []*modelv1.TagValue{
					{
						Value: &modelv1.TagValue_BinaryData{
							BinaryData: bb,
						},
					},
				},
			},
			{
				Tags: searchableTags,
			},
		},
	}
	return e
}

func getTag(tag interface{}) *modelv1.TagValue {
	if tag == nil {
		return &modelv1.TagValue{
			Value: &modelv1.TagValue_Null{},
		}
	}
	switch t := tag.(type) {
	case int:
		return &modelv1.TagValue{
			Value: &modelv1.TagValue_Int{
				Int: &modelv1.Int{
					Value: int64(t),
				},
			},
		}
	case string:
		return &modelv1.TagValue{
			Value: &modelv1.TagValue_Str{
				Str: &modelv1.Str{
					Value: t,
				},
			},
		}
	}
	return nil
}

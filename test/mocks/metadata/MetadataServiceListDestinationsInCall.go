// Copyright (c) 2016 Uber Technologies, Inc.
// 
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.


package metadata

import "github.com/uber/cherami-server/.generated/go/shared"
import "github.com/stretchr/testify/mock"

// MetadataServiceListDestinationsInCall is an autogenerated mock type for the MetadataServiceListDestinationsInCall type
type MetadataServiceListDestinationsInCall struct {
	mock.Mock
}

// Done provides a mock function with given fields:
func (_m *MetadataServiceListDestinationsInCall) Done() error {
	ret := _m.Called()

	var r0 error
	if rf, ok := ret.Get(0).(func() error); ok {
		r0 = rf()
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// Flush provides a mock function with given fields:
func (_m *MetadataServiceListDestinationsInCall) Flush() error {
	ret := _m.Called()

	var r0 error
	if rf, ok := ret.Get(0).(func() error); ok {
		r0 = rf()
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetResponseHeaders provides a mock function with given fields: headers
func (_m *MetadataServiceListDestinationsInCall) SetResponseHeaders(headers map[string]string) error {
	ret := _m.Called(headers)

	var r0 error
	if rf, ok := ret.Get(0).(func(map[string]string) error); ok {
		r0 = rf(headers)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// Write provides a mock function with given fields: arg
func (_m *MetadataServiceListDestinationsInCall) Write(arg *shared.DestinationDescription) error {
	ret := _m.Called(arg)

	var r0 error
	if rf, ok := ret.Get(0).(func(*shared.DestinationDescription) error); ok {
		r0 = rf(arg)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

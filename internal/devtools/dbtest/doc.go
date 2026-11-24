// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package dbtest starts Postgres and MongoDB containers for integration
// tests through testcontainers-go.
//
// Everything except this file is built only with the integration build tag,
// so ordinary builds and unit tests never need Docker. Run the integration
// tests with:
//
//	make test-integration
//
// The engine versions under test come from the environment:
//
//	PENTYM_TEST_POSTGRES  comma-separated Postgres major versions (default 16,17,18)
//	PENTYM_TEST_MONGODB   comma-separated MongoDB versions (default 7.0,8.0,9.0)
//
// MongoDB always runs as a single-node replica set.
package dbtest

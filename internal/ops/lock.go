// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package ops

import (
	"context"

	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// WithLock runs fn holding the repository's shared or exclusive lock,
// refreshed while fn runs and released afterwards (SDD 7.1).
func WithLock(ctx context.Context, r *repo.Repository, exclusive bool, fn func() error) error {
	id := repo.CurrentIdentity()
	m := lock.NewLocks(r.Backend(), lock.CurrentHolder(id.User, id.Host, ""))
	acquire := m.AcquireShared
	if exclusive {
		acquire = m.AcquireExclusive
	}
	l, err := acquire(ctx)
	if err != nil {
		return err
	}
	stop := l.KeepFresh(ctx, lock.LockRefresh)
	err = fn()
	stop()
	if rerr := l.Release(context.WithoutCancel(ctx)); err == nil {
		err = rerr
	}
	return err
}

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package configreload

// Result describes the confirmation boundary of one component's synchronous reload.
type Result string

const (
	// Accepted means the component applied the changes; they are recorded as
	// confirmed and drop out of the pending set. It is returned only after the
	// effect is published and verified, so confirmation never precedes publication
	// and no convergence window follows it.
	Accepted Result = "Accepted"
	// NotNecessary means the changes required no effect; they are confirmed
	// exactly like Accepted.
	NotNecessary Result = "NotNecessary"
	// Rejected leaves confirmed inputs unchanged and adds no pending fields.
	// Fields left uncertain by an earlier error remain pending for a later retry.
	Rejected Result = "Rejected"

	AdminKey      = "E2B_ADMIN_KEY"
	DSN           = "E2B_KEY_STORAGE_DSN"
	Pepper        = "E2B_KEY_HASH_PEPPER"
	RedisUsername = "QUOTA_REDIS_USERNAME"
	RedisPassword = "QUOTA_REDIS_PASSWORD"
)

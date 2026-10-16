-- SPDX-License-Identifier: AGPL-3.0-only
-- The decision statistics of people (DIG-4): a point of farm.proposals.accept_share or
-- farm.proposals.decline_reason_share is of a role and keeps the proposal category as its
-- second dimension, so code computes the shares by role and by category.
ALTER TABLE obs_points ADD COLUMN dim text;

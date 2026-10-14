-- SPDX-License-Identifier: AGPL-3.0-only
-- Money for tool calls (BUD-2, BUD-3, BUD-5) and the purpose of a usage line (BUD-7): a tool
-- call reserves its manifest price in a spend row linked to the call.

ALTER TABLE spend ADD COLUMN purpose text;
ALTER TABLE spend ADD COLUMN toolcall text;
CREATE INDEX spend_toolcall ON spend (toolcall) WHERE toolcall IS NOT NULL;
CREATE INDEX spend_tenant_line_time ON spend (tenant, line, time);

-- A call that ends settles its reserve in the same transaction, whatever path ended it: a call
-- a toolhost took is charged at its price, one nobody took gives its reserve back.
CREATE FUNCTION toolcall_settle_spend() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.lease_no > 0 THEN
        UPDATE spend SET state = 'committed' WHERE toolcall = NEW.id AND state = 'reserved';
    ELSE
        DELETE FROM spend WHERE toolcall = NEW.id AND state = 'reserved';
    END IF;
    RETURN NULL;
END
$$;
CREATE TRIGGER toolcall_settle_spend AFTER UPDATE OF state ON toolcalls
    FOR EACH ROW WHEN (NEW.state NOT IN ('requested', 'queued', 'leased') AND OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION toolcall_settle_spend();

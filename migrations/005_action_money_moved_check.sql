-- Prevent actions from leaving the proposed state when their audit claims money moved.
-- NOT VALID keeps legacy rows untouched while enforcing the rule on new/updated rows.
ALTER TABLE actions
    ADD CONSTRAINT actions_no_money_moved_after_proposal_check
    CHECK (
        status = 'proposed'
        OR NOT jsonb_path_exists(result, '$.**.money_moved ? (@ == true)')
    ) NOT VALID;

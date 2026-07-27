DO $$
BEGIN
    IF to_regclass('public.token_theft_pending_revocations') IS NULL THEN
        RETURN;
    END IF;

    LOCK TABLE public.token_theft_pending_revocations IN ACCESS EXCLUSIVE MODE;
    IF EXISTS (SELECT 1 FROM public.token_theft_pending_revocations) THEN
        RAISE EXCEPTION
            'cannot drop completed_at while token theft revocations remain pending';
    END IF;
END
$$;

ALTER TABLE IF EXISTS token_theft_pending_revocations
    DROP COLUMN IF EXISTS completed_at;

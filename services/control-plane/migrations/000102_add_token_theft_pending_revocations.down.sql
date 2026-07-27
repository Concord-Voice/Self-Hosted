-- Refuse a destructive rollback while theft-triggered revocations remain pending.
DO $$
DECLARE
    has_pending BOOLEAN;
BEGIN
    IF to_regclass('public.token_theft_pending_revocations') IS NULL THEN
        RETURN;
    END IF;

    BEGIN
        EXECUTE
            'LOCK TABLE public.token_theft_pending_revocations IN ACCESS EXCLUSIVE MODE';
    EXCEPTION
        WHEN undefined_table THEN
            RETURN;
    END;

    EXECUTE
        'SELECT EXISTS (SELECT 1 FROM public.token_theft_pending_revocations)'
        INTO has_pending;
    IF has_pending THEN
        RAISE EXCEPTION
            'cannot drop token_theft_pending_revocations while theft revocations remain pending';
    END IF;

    EXECUTE 'DROP TABLE IF EXISTS public.token_theft_pending_revocations';
END
$$;

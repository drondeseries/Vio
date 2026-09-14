-- +goose Up
-- +goose StatementBegin
CREATE TABLE public.user_device_profiles (
    user_id integer NOT NULL,
    profile_id text NOT NULL,
    device_id text NOT NULL,
    codecs_video text[] NOT NULL DEFAULT '{}',
    codecs_audio text[] NOT NULL DEFAULT '{}',
    containers text[] NOT NULL DEFAULT '{}',
    max_resolution text NOT NULL DEFAULT '',
    hdr boolean NOT NULL DEFAULT false,
    source text NOT NULL DEFAULT 'client',
    capability_fingerprint text NOT NULL DEFAULT '',
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    last_reported_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_device_profiles_pkey PRIMARY KEY (user_id, profile_id, device_id),
    CONSTRAINT user_device_profiles_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE,
    CONSTRAINT user_device_profiles_profile_fkey
        FOREIGN KEY (user_id, profile_id) REFERENCES public.user_profiles(user_id, id) ON DELETE CASCADE,
    CONSTRAINT user_device_profiles_source_check CHECK (source IN ('client', 'admin', 'seed'))
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS public.user_device_profiles;
-- +goose StatementEnd

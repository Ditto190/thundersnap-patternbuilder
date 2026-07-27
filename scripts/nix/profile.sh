# Nix activation for login shells.
if [ -n "${HOME-}" ]; then
    if [ -n "${XDG_STATE_HOME-}" ]; then
        user_profile="$XDG_STATE_HOME/nix/profile"
    else
        user_profile="$HOME/.local/state/nix/profile"
    fi
    [ -e "$user_profile" ] || user_profile="$HOME/.nix-profile"

    export NIX_PROFILES="/nix/var/nix/profiles/default $user_profile${NIX_PROFILES:+ $NIX_PROFILES}"
    export PATH="$user_profile/bin:/nix/var/nix/profiles/default/bin:$PATH"
    export NIX_PATH="$HOME/.nix-defexpr/channels:/nix/var/nix/profiles/per-user/root/channels${NIX_PATH:+:$NIX_PATH}"

    if [ ! -e "$HOME/.nix-defexpr/channels" ] && [ -t 1 ]; then
        printf '%s\n' \
            'Nix is using the system nixpkgs channel.' \
            'To silence the missing per-user channels warning, run:' \
            '  mkdir -p "$HOME/.nix-defexpr/channels"'
    fi

    if [ -z "${XDG_DATA_DIRS-}" ]; then
        export XDG_DATA_DIRS="/usr/local/share:/usr/share:$user_profile/share:/nix/var/nix/profiles/default/share"
    else
        export XDG_DATA_DIRS="$XDG_DATA_DIRS:$user_profile/share:/nix/var/nix/profiles/default/share"
    fi
    if [ -e /etc/ssl/certs/ca-certificates.crt ]; then
        export NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
    fi
    unset user_profile
fi

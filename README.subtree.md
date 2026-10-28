# libvirt package setup with git subtree

Summary of work done for OBS project/package **`Devel:Virt:SLE-16:git/libvirt`**.  
The repository `atartamo/libvirt` is a fork of `pool/libvirt`.

> **Note:** This setup is currently a proof of concept. We need to test and stress it more before switching to it in production.

---

## What Evergit does (brief overview)

Evergit is a Git proxy that converts upstream Git repositories from SHA-1 to SHA-256 on the fly.  
It allows SHA-256 systems (like SUSE Gitea / OBS) to consume upstream Git repositories and submodules without manual migration.

---

## 1. Steps performed

### Export SLE patches
Run in `virtualization/libvirt`:
```bash
git format-patch v11.4.0..v11.4.0-sle16.0 -o /tmp/libvirt-patches/
```

### Clone fork and switch branch
```bash
git clone gitea@src.suse.de:atartamo/libvirt.git libvirt-atartamo
cd libvirt-atartamo
git checkout slfo-1.2
```

### Remove PNG from Git LFS (before adding subtree)
Remove `*.png filter=lfs ...` from `.gitattributes` **before** importing upstream code.  
If not done first, Gitea Git LFS server hooks fail when pushing upstream PNG files.

### Import upstream and apply patches
```bash
git remote add upstream ssh://git@evergit.suse.de/github.com/libvirt/libvirt.git
git subtree add --prefix libvirt upstream v11.4.0 --squash
git am --directory=libvirt /tmp/libvirt-patches/*.patch
```

### Add root `.gitmodules`
Git only checks `.gitmodules` at the repository root. The file `libvirt/.gitmodules` inside the subtree directory is ignored by Git, even with `insteadOf` rules.

Create `.gitmodules` at repository root:
```ini
[submodule "keycodemapdb"]
       path = libvirt/subprojects/keycodemapdb
       url = https://evergit.suse.de/gitlab.com/keycodemap/keycodemapdb.git
```

---

## 2. Changes compared to original `pool/libvirt`

* **`.gitattributes`:** Removed LFS filter for `*.png`.
* **`.gitmodules`:** Added at repository root for `keycodemapdb`.
* **Removed OBS service & LFS tarball:** Deleted `_service`, `libvirt-11.4.0.obscpio`, and `libvirt.obsinfo`.
* **`libvirt.spec`:** Added `#!CreateArchive: %{name}` so OBS creates the source archive directly from the `libvirt/` subtree directory.

---

## 3. Future workflow

### Pull new upstream releases
```bash
git subtree pull --prefix libvirt upstream <new-tag> --squash
```

### Cherry-pick upstream commits
Upstream `libvirt` uses SHA-1, whereas our fork and OBS packaging branch use SHA-256.  
Use `git from-sha1` (which supports short hashes of 7+ characters) combined with `-X subtree=libvirt` so Git shifts the patch into the subtree prefix:

```bash
# 1. Fetch latest upstream commits via Evergit
git fetch upstream

# 2. Cherry-pick using subtree strategy and translated SHA-256
git cherry-pick -X subtree=libvirt -xse $(git from-sha1 7b889af2d2)
# Or explicitly pass the remote if evergit.remote is not set:
# git cherry-pick -X subtree=libvirt -xse $(git from-sha1 7b889af2d2 upstream)
```

In the editor opened by `-e`, keep the recorded cherry-picked commit line and add the original upstream SHA-1 commit reference per packaging guidelines:
```text
Upstream-commit: 7b889af2d235d3fadfc172f371f02df0b74e40f2
```

### Send changes upstream
Use `git subtree split` to isolate our commits into a clean branch:
```bash
git subtree split --prefix libvirt -b send-upstream
```

---

## 4. Evergit notes

### `gnulib` submodule
* libvirt references `gnulib` as a submodule from Savannah.
* It is not used in our build.
* In Evergit, `gnulib` is marked as **synthetic** in `-synthetic-gitlinks`. Evergit provides synthetic SHA-256 gitlinks without cloning or converting the gnulib repository.

### Client configuration (`setup.sh`)
To clone upstream repositories through Evergit cache without changing URLs manually, use `setup.sh`.  
Use `--dir` to limit the rules to a specific folder and avoid changing global git configuration:
```bash
curl -fsSL http://localhost:8080/setup.sh | bash -s -- --dir ~/repodir --ssh
```

---

## 5. Next steps proposal

* Create a `virtualization` organization on Gitea.
* Add OBS build status badges in `README.md`, similar to `https://src.opensuse.org/javascript/_ObsPrj`.

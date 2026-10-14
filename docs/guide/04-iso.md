# 4. Building the ISO and the USB stick
Summary: Build the installer ISO, copy it to Windows, and write a USB stick.

The ISO is built on a Debian 13 machine (a P8S OS server or any Debian VM) that has the P8S OS source. Everything the install needs is on it, so no internet is needed during the install.

**On the build machine** (after pulling the latest code):

```
cd ~/P8SOS
git pull
sudo bash iso/build-installer.sh ~/P8SOS/dist
```

- First build: 30–60 minutes and about 15 GB free (it downloads \~1.5 GB of packages once and compiles ZFS). Later builds reuse the downloads.
- Result: `dist/p8sos-installer-YYYYMMDD-amd64.iso` (about 2 GB) and a `.sha256` file.
- Don't use `make-iso.sh` on a running P8S OS server: it stops and removes VMs.

**Copy it to Windows** (PowerShell on the Windows PC):

```
scp USER@BUILD-MACHINE:P8SOS/dist/p8sos-installer-*.iso $HOME\Downloads\
```

**USB stick for a real server:** Rufus → select the ISO → when asked, choose **DD Image** mode → Start. Everything on the stick is erased.

**Unattended ISO** (many identical servers or test VMs): put an answers file next to the build and set `ANSWERS=/path/answers.json` when building. The result is named `...-auto-amd64.iso` and **erases the listed disks 10 seconds after it boots** — label that stick clearly.

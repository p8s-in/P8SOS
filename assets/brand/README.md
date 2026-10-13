# P8S logo and where P8S OS uses it

Masters: `svg/` (all variants, brand blue `#014578`) and `master/` (8K PNGs,
white on transparent). Pack notes: README-pack.md.

| Where | Files | Put there by |
|---|---|---|
| Installer USB/CD boot menu (BIOS and UEFI) | `iso/splash.png`, `iso/grub-theme.txt` | `iso/build-iso.sh` |
| Server boot menu (GRUB) | `grub/` → `/boot/grub/themes/p8s` | `scripts/branding.sh` |
| Boot splash while the server starts (Plymouth; Esc shows messages) | `plymouth/` → `/usr/share/plymouth/themes/p8s` | `scripts/branding.sh` |
| Console login screen, SSH welcome | ASCII logo in `/etc/issue`, `/etc/update-motd.d/05-p8s` | `scripts/branding.sh` |
| Dashboard: tab icon, phone home screen, header, sign-in page, console pages | `internal/web/brand/` | embedded in p8sd |

The boot images are made from the masters with Pillow:

    python3 assets/brand/make-boot-images.py

No boot splash on a server: `sudo touch /etc/p8s/no-splash && sudo bash update.sh`
(boot messages scroll as before). A server rebranded for a reseller (P8S Cloud
brand name not starting with "P8S") hides the P8S logo in the dashboard.

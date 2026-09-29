importpath=github.com/daaku/snoozer
pkgname=$(basename "$importpath")
pkgver=$(git rev-list --count HEAD)
pkgrel=1
pkgdesc='Alarm clock for Linux using zenity dialogs and mpv'
arch=('x86_64')
url="https://$importpath"
license=('MIT')
depends=(
  'glibc'  # libc
  'mpv'    # looping alarm audio
  'zenity' # alarm dialogs
)
makedepends=('go')

build() {
  cd ..
  go build -trimpath -o snoozer .
}

package() {
  cd ..
  install -Dm755 snoozer "$pkgdir/usr/bin/snoozer"
  install -Dm644 snoozer.service "$pkgdir/usr/lib/systemd/user/snoozer.service"
  install -Dm644 license "$pkgdir/usr/share/licenses/$pkgname/LICENSE"
}

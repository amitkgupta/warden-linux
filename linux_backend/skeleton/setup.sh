#!/bin/bash

[ -n "$DEBUG" ] && set -o xtrace
set -o nounset
set -o errexit
shopt -s nullglob

cd $(dirname $0)

# Defaults for debugging the setup script
iface_name="${UNIQUENESS_TAG}$(echo ${id} | cut -b $(expr ${#UNIQUENESS_TAG} + 1)-)"
id=${id:-test}
network_host_ip=${network_host_ip:-10.0.0.1}
network_host_iface="w-${iface_name}-0"
network_container_ip=${network_container_ip:-10.0.0.2}
network_container_iface="w-${iface_name}-1"
user_uid=${user_uid:-10000}
rootfs_path=$(readlink -f $rootfs_path)

# Write configuration
cat > etc/config <<-EOS
id=$id
network_host_ip=$network_host_ip
network_host_iface=$network_host_iface
network_container_ip=$network_container_ip
network_container_iface=$network_container_iface
user_uid=$user_uid
rootfs_path=$rootfs_path
EOS

# Strip /dev down to the bare minimum
rm -rf $rootfs_path/dev/*

# /dev/tty
file=$rootfs_path/dev/tty
mknod -m 666 $file c 5 0
chown root:tty $file

# /dev/random, /dev/urandom
file=$rootfs_path/dev/random
mknod -m 666 $file c 1 8
chown root:root $file
file=$rootfs_path/dev/urandom
mknod -m 666 $file c 1 9
chown root:root $file

# /dev/null, /dev/zero
file=$rootfs_path/dev/null
mknod -m 666 $file c 1 3
chown root:root $file
file=$rootfs_path/dev/zero
mknod -m 666 $file c 1 5
chown root:root $file

# /dev/fd, /dev/std{in,out,err}
pushd $rootfs_path/dev > /dev/null
ln -s /proc/self/fd
ln -s fd/0 stdin
ln -s fd/1 stdout
ln -s fd/2 stderr
popd > /dev/null

cat > $rootfs_path/etc/hostname <<-EOS
$id
EOS

cat > $rootfs_path/etc/hosts <<-EOS
127.0.0.1 localhost
$network_container_ip $id
EOS

# By default, inherit the nameserver from the host container.
#
# Exception: When the host's nameserver is set to localhost (127.0.0.1), it is
# assumed to be running its own DNS server and listening on all interfaces.
# In this case, the warden container must use the network_host_ip address
# as the nameserver.
if [[ "$(cat /etc/resolv.conf)" == "nameserver 127.0.0.1" ]]
then
  cat > $rootfs_path/etc/resolv.conf <<-EOS
nameserver $network_host_ip
EOS
else
  cp /etc/resolv.conf $rootfs_path/etc/
fi

# Add vcap user if not already present
$(which chroot) $rootfs_path env -i /bin/bash -l <<-EOS
if ! id vcap > /dev/null 2>&1
then
  useradd -mU -u $user_uid -s /bin/bash vcap
fi
EOS

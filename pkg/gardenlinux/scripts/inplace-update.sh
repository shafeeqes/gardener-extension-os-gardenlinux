#!/bin/bash

set -Eeuo pipefail

# Check if a version argument is passed
if [ "$#" -ne 1 ]; then
    echo "Usage: $0 <version>"
    exit 1
fi

VERSION=$1

# Run the update with the specified version
sudo gardenlinux-update "$VERSION"
sudo reboot

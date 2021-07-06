#!/bin/bash

echo Deploying updated documentation

# Sleep for dramatic purposes!

sleep 5

generate-md --layout github --input ./ --output /var/www/kube-vip/

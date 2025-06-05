#!/usr/bin/bash

rm -f logs.txt

for i in $(seq 1 100);
do
  echo "RUN $i"
  GOMAXPROCS=4 make e2e-tests129
  if [[ "$?" -ne 0 ]]; then 
    echo "FAILED AT RUN $i"
    break
  fi
done

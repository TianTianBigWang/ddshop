#!/bin/bash


REPOSITORY=${REPOSITORY:-"zc2638/ddshop"}
TAG=${TAG:-"latest"}
ARCHS=(amd64 arm64 arm)
TAGS=()

IFS=',' read -r -a TAGS <<< "$TAG"

#for tag in "${TAGS[@]}"
#do
#    echo "$tag"
#done

for arch in "${ARCHS[@]}" ; do
  for tag in "${TAGS[@]}"; do
    docker pull --platform "$arch" "$REPOSITORY":"$tag"
    docker tag "$REPOSITORY":"$tag" "$REPOSITORY":"$tag"-"$arch"
  done
done

for tag in "${TAGS[@]}"; do
  manifestCreateCmd="docker manifest create $REPOSITORY:$tag"
  for arch in "${ARCHS[@]}" ; do
    docker push "$REPOSITORY":"$tag"-"$arch"
    manifestCreateCmd="$manifestCreateCmd $REPOSITORY:$tag-$arch"
  done
  doCreate="$($manifestCreateCmd)"
  echo "$doCreate"
  docker manifest push "$REPOSITORY":"$tag"
done



# Huawei input reader

Build with a JDK and Android D8 from the build tools. Run from the repository root with an empty build directory.

```sh
javac --release 8 -d "$build" tvd/huawei-input/HuaweiInput.java
d8 --min-api 28 --output "$build" "$build/HuaweiInput.class"
install -m 0644 "$build/classes.dex" tvd/huawei-input.dex
```

The Huawei daemon copies the DEX to the configured television if the remote helper is missing.
It reads the active source and checks signal status from the local Huawei TV service.
The source mapping was measured on the supported Huawei Vision firmware. An unrecognized or unreadable source remains unverified.

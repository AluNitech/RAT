# FFmpeg/FFplay Runtime Bundles

Place platform-specific builds of FFmpeg here so the Makefile can bundle them with the ModernRat binaries. The recommended (and automated) way is to run:

```
bash scripts/fetch_ffmpeg.sh linux
bash scripts/fetch_ffmpeg.sh windows
```

This script downloads the following upstream bundles, extracts `ffmpeg`/`ffplay`, and stores them under `third_party/ffmpeg/<platform>/`:

- Linux: https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-linux64-gpl.tar.xz
- Windows: https://www.gyan.dev/ffmpeg/builds/ffmpeg-git-essentials.7z

Directory layout after fetching:

```
third_party/ffmpeg/
├── linux/
│   ├── ffmpeg
│   └── ffplay
└── windows/
    ├── ffmpeg.exe
    └── ffplay.exe
```

The build targets copy these files next to the generated executables. If you prefer manual control, download the archives yourself, extract the binaries, and place them in the matching folder before running `make`.

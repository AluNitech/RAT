# FFmpeg/FFplay Runtime Bundles

Place platform-specific builds of FFmpeg here so the Makefile can bundle them with the ModernRat binaries.

```
third_party/ffmpeg/
├── linux/
│   ├── ffmpeg
│   └── ffplay
└── windows/
    ├── ffmpeg.exe
    └── ffplay.exe
```

The build targets copy these files next to the generated executables. Download the official builds from https://ffmpeg.org/download.html, extract the binaries, and drop them into the matching folder before building.

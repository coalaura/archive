# archive

A small streaming archival tool. Remote files are read from the network and written directly through tar and Zstandard into the final archive; model files are never staged uncompressed on disk.

## Usage

```text
archive huggingface Qwen/Qwen3.8-27B
archive huggingface Qwen/Qwen3.8-27B --revision main
archive hf Qwen/Qwen3.8-27B -r <commit-or-tag>
```

The binary reads `config.yml` from the current working directory:

```yaml
tokens:
  huggingface: "hf_..."
```

The config file is optional for public repositories.

Archives are written in the current working directory under:

```text
archive/
  huggingface/
    Qwen_Qwen3.8-27B@<commit>.tar.zst
```

Each repository file is placed under `repository/` in the tar archive. `.archive/manifest.json` contains the pinned Hugging Face commit plus SHA-256 hashes calculated while streaming each file.

## Failure handling

Each tar entry is compressed as an independent concatenated Zstandard frame. If a transfer fails, only the current frame is truncated and retried.

A small hidden state file checkpoints completed entries. Re-running the same command resumes the pinned commit from the last fully synced file instead of starting the archive from scratch.

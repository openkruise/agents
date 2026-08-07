from setuptools import setup, find_packages

with open("README.md", "r", encoding="utf-8") as fh:
    long_description = fh.read()

setup(
    name="kruise-agents",
    version="0.1.0",
    author="OpenKruise",
    author_email="",
    description="A customized E2B SDK patch that converts the native E2B protocol to the OpenKruise Agents private protocol",
    long_description=long_description,
    long_description_content_type="text/markdown",
    url="https://github.com/openkruise/agents",
    packages=find_packages(),
    classifiers=[
        "Development Status :: 3 - Alpha",
        "Intended Audience :: Developers",
        "License :: OSI Approved :: Apache Software License",
        "Operating System :: OS Independent",
        "Programming Language :: Python :: 3",
        "Programming Language :: Python :: 3.9",
        "Programming Language :: Python :: 3.10",
        "Programming Language :: Python :: 3.11",
    ],
    python_requires=">=3.9,<4.0",
    install_requires=[
        "e2b>=2.8.0",
        "e2b-code-interpreter>=2.4.1",
    ],
    extras_require={
        "traffic-token-refresh": [
            "e2b>=2.35.0,<2.38.0",
            "e2b-code-interpreter>=2.9.0,<2.10.0",
        ],
        "dev": [
            "e2b>=2.35.0,<2.38.0",
            "e2b-code-interpreter>=2.9.0,<2.10.0",
            "pytest>=6.2.5",
            "pytest-asyncio>=0.21.0",
            "pytest-cov>=2.12.1",
        ],
    },
)
